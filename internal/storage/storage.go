// Package storage abstracts where uploaded PDFs live. The service ships a
// local-disk implementation; the interface exists so an Azure Blob backend
// can be dropped in later without touching handlers or the pipeline.
package storage

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get and Delete when a key has no object.
var ErrNotFound = errors.New("storage: object not found")

// BlobStore stores opaque byte blobs under caller-chosen keys.
//
// Keys are slash-separated paths such as "uploads/<upload-id>/<file-id>.pdf".
// Implementations must treat them as opaque and must not let a key escape
// the store's own namespace.
type BlobStore interface {
	// Put stores everything readable from r under key, replacing any
	// existing object, and reports how many bytes were written.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)

	// Get opens the object at key. The caller closes the reader. It
	// returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Delete removes the object at key. Deleting a key that does not
	// exist returns ErrNotFound.
	Delete(ctx context.Context, key string) error
}
