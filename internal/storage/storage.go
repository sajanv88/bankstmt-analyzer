// Package storage abstracts where uploaded PDFs live. The service ships a
// local-disk implementation; the interface exists so an Azure Blob backend
// can be dropped in later without touching handlers or the pipeline.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
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

// validateKey checks a store key against the rules every implementation
// enforces, and returns it cleaned into slash-separated form.
//
// It lives here rather than in one implementation because the two must
// agree: a key the local store rejects must not be quietly accepted by the
// object store, or the same upload would behave differently depending on
// which backend is configured.
func validateKey(key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("storage: key must not be empty")
	}
	// Reject leading separators and drive letters before consulting the
	// filesystem. On Windows filepath.IsAbs("/etc/passwd") is false, so
	// relying on the OS alone would quietly rewrite a Unix-style absolute
	// key into a relative one there while rejecting it on Linux. Keys mean
	// the same thing on every platform, so the check has to too.
	if strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) || hasDriveLetter(key) {
		return "", fmt.Errorf("storage: key %q escapes the store root", key)
	}

	// Keys are slash-separated; normalise a Windows separator first so it
	// cannot sneak past the traversal check.
	clean := path.Clean(strings.ReplaceAll(key, `\`, "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("storage: key %q escapes the store root", key)
	}
	return clean, nil
}

// hasDriveLetter reports whether key begins with a Windows drive specifier
// such as "C:". filepath.VolumeName only recognises one when running on
// Windows, so testing for it directly keeps a key valid or invalid on every
// platform alike.
func hasDriveLetter(key string) bool {
	if len(key) < 2 || key[1] != ':' {
		return false
	}
	c := key[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
