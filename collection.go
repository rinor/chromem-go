package chromem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"sync"
)

// Collection represents a collection of documents.
// It also has a configured embedding function, which is used when adding documents
// that don't have embeddings yet.
type Collection struct {
	Name string

	persistDirectory string
	metadata         map[string]string
	documents        map[string]*Document
	documentsLock    sync.RWMutex
	embed            EmbeddingFunc
}

// We don't export this yet to keep the API surface to the bare minimum.
// Users create collections via [Client.CreateCollection].
func newCollection(name string, metadata map[string]string, embed EmbeddingFunc, dir string) (*Collection, error) {
	// We copy the metadata to avoid data races in case the caller modifies the
	// map after creating the collection while we range over it.
	m := make(map[string]string, len(metadata))
	for k, v := range metadata {
		m[k] = v
	}

	c := &Collection{
		Name: name,

		metadata:  m,
		documents: make(map[string]*Document),
		embed:     embed,
	}

	// Persistence
	if dir != "" {
		safeName := hash2hex(name)
		c.persistDirectory = path.Join(dir, safeName)
		// Create dir
		err := os.MkdirAll(c.persistDirectory, 0o700)
		if err != nil {
			return nil, fmt.Errorf("couldn't create collection directory: %w", err)
		}
		// Persist name and metadata
		metadataPath := path.Join(c.persistDirectory, metadataFileName)
		pc := struct {
			Name     string
			Metadata map[string]string
		}{
			Name:     name,
			Metadata: m,
		}
		err = persist(metadataPath, pc)
		if err != nil {
			return nil, fmt.Errorf("couldn't persist collection metadata: %w", err)
		}
	}

	return c, nil
}

// Add embeddings to the datastore.
//
//   - ids: The ids of the embeddings you wish to add
//   - embeddings: The embeddings to add. If nil, embeddings will be computed based
//     on the contents using the embeddingFunc set for the Collection. Optional.
//   - metadatas: The metadata to associate with the embeddings. When querying,
//     you can filter on this metadata. Optional.
//   - contents: The contents to associate with the embeddings.
//
// This is a Chroma-like method. For a more Go-idiomatic one, see [AddDocuments].
func (c *Collection) Add(ctx context.Context, ids []string, embeddings [][]float32, metadatas []map[string]string, contents []string) error {
	return c.AddConcurrently(ctx, ids, embeddings, metadatas, contents, 1)
}

// AddConcurrently is like Add, but adds embeddings concurrently.
// This is mostly useful when you don't pass any embeddings so they have to be created.
// Upon error, concurrently running operations are canceled and the error is returned.
//
// This is a Chroma-like method. For a more Go-idiomatic one, see [AddDocuments].
func (c *Collection) AddConcurrently(ctx context.Context, ids []string, embeddings [][]float32, metadatas []map[string]string, contents []string, concurrency int) error {
	if len(ids) == 0 {
		return errors.New("ids are empty")
	}
	if len(embeddings) == 0 && len(contents) == 0 {
		return errors.New("either embeddings or contents must be filled")
	}
	if len(embeddings) != 0 {
		if len(embeddings) != len(ids) {
			return errors.New("ids and embeddings must have the same length")
		}
	} else {
		// Assign empty slice so we can simply access via index later
		embeddings = make([][]float32, len(ids))
	}
	if len(metadatas) != 0 {
		if len(ids) != len(metadatas) {
			return errors.New("when metadatas is not empty it must have the same length as ids")
		}
	} else {
		// Assign empty slice so we can simply access via index later
		metadatas = make([]map[string]string, len(ids))
	}
	if len(contents) != 0 {
		if len(contents) != len(ids) {
			return errors.New("ids and contents must have the same length")
		}
	} else {
		// Assign empty slice so we can simply access via index later
		contents = make([]string, len(ids))
	}
	if concurrency < 1 {
		return errors.New("concurrency must be at least 1")
	}

	// Convert Chroma-style parameters into a slice of documents.
	docs := make([]Document, 0, len(ids))
	for i, id := range ids {
		docs = append(docs, Document{
			ID:        id,
			Metadata:  metadatas[i],
			Embedding: embeddings[i],
			Content:   contents[i],
		})
	}

	return c.AddDocuments(ctx, docs, concurrency)
}

// AddDocuments adds documents to the collection with the specified concurrency.
// If the documents don't have embeddings, they will be created using the collection's
// embedding function.
// Upon error, concurrently running operations are canceled and the error is returned.
func (c *Collection) AddDocuments(ctx context.Context, documents []Document, concurrency int) error {
	if len(documents) == 0 {
		// TODO: Should this be a no-op instead?
		return errors.New("documents slice is nil or empty")
	}
	if concurrency < 1 {
		return errors.New("concurrency must be at least 1")
	}
	// For other validations we rely on AddDocument.

	var globalErr error
	globalErrLock := sync.Mutex{}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	setGlobalErr := func(err error) {
		globalErrLock.Lock()
		defer globalErrLock.Unlock()
		// Another goroutine might have already set the error.
		if globalErr == nil {
			globalErr = err
			// Cancel the operation for all other goroutines.
			cancel(globalErr)
		}
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, concurrency)
	for _, doc := range documents {
		wg.Add(1)
		go func(doc Document) {
			defer wg.Done()

			// Don't even start if another goroutine already failed.
			if ctx.Err() != nil {
				return
			}

			// Wait here while $concurrency other goroutines are creating documents.
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			err := c.AddDocument(ctx, doc)
			if err != nil {
				setGlobalErr(fmt.Errorf("couldn't add document '%s': %w", doc.ID, err))
				return
			}
		}(doc)
	}

	wg.Wait()

	return globalErr
}

// AddDocument adds a document to the collection.
// If the document doesn't have an embedding, it will be created using the collection's
// embedding function.
func (c *Collection) AddDocument(ctx context.Context, doc Document) error {
	if doc.ID == "" {
		return errors.New("document ID is empty")
	}
	if len(doc.Embedding) == 0 && doc.Content == "" {
		return errors.New("either document embedding or content must be filled")
	}

	// We copy the metadata to avoid data races in case the caller modifies the
	// map after creating the document while we range over it.
	m := make(map[string]string, len(doc.Metadata))
	for k, v := range doc.Metadata {
		m[k] = v
	}

	// Create embedding if they don't exist
	if len(doc.Embedding) == 0 {
		embedding, err := c.embed(ctx, doc.Content)
		if err != nil {
			return fmt.Errorf("couldn't create embedding of document: %w", err)
		}
		doc.Embedding = embedding
	}

	c.documentsLock.Lock()
	// We don't defer the unlock because we want to do it earlier.
	c.documents[doc.ID] = &doc
	c.documentsLock.Unlock()

	// Persist the document
	if c.persistDirectory != "" {
		safeID := hash2hex(doc.ID)
		filePath := path.Join(c.persistDirectory, safeID)
		err := persist(filePath, doc)
		if err != nil {
			return fmt.Errorf("couldn't persist document: %w", err)
		}
	}

	return nil
}

// Count returns the number of documents in the collection.
func (c *Collection) Count() int {
	c.documentsLock.RLock()
	defer c.documentsLock.RUnlock()
	return len(c.documents)
}

// Performs a nearest neighbors query on a collection specified by UUID.
//
//   - queryText: The text to search for.
//   - nResults: The number of results to return. Must be > 0.
//   - where: Conditional filtering on metadata. Optional.
//   - whereDocument: Conditional filtering on documents. Optional.
func (c *Collection) Query(ctx context.Context, queryText string, nResults int, where, whereDocument map[string]string) ([]Result, error) {
	if queryText == "" {
		return nil, errors.New("queryText is empty")
	}
	if nResults <= 0 {
		return nil, errors.New("nResults must be > 0")
	}

	c.documentsLock.RLock()
	defer c.documentsLock.RUnlock()
	if len(c.documents) == 0 {
		return nil, nil
	}

	// Validate whereDocument operators
	for k := range whereDocument {
		if !slices.Contains(supportedFilters, k) {
			return nil, errors.New("unsupported operator")
		}
	}

	// Filter docs by metadata and content
	filteredDocs := filterDocs(c.documents, where, whereDocument)

	// No need to continue if the filters got rid of all documents
	if len(filteredDocs) == 0 {
		return nil, nil
	}

	queryVectors, err := c.embed(ctx, queryText)
	if err != nil {
		return nil, fmt.Errorf("couldn't create embedding of query: %w", err)
	}

	// For the remaining documents, calculate cosine similarity.
	res, err := calcDocSimilarity(ctx, queryVectors, filteredDocs)
	if err != nil {
		return nil, fmt.Errorf("couldn't calculate cosine similarity: %w", err)
	}

	// Sort by similarity
	sort.Slice(res, func(i, j int) bool {
		// The `less` function would usually use `<`, but we want to sort descending.
		return res[i].Similarity > res[j].Similarity
	})

	// Return the top nResults
	return res[:nResults], nil
}
