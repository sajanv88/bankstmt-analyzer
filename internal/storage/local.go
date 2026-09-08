package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Local is a BlobStore backed by a directory on the local filesystem.
// Writes are atomic: content lands in a temporary file that is renamed
// into place, so a crash mid-upload never leaves a truncated PDF behind.
type Local struct {
	root string
}

// NewLocal creates the root directory if needed and returns a store
// rooted at it.
func NewLocal(root string) (*Local, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage: local root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve root %q: %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("storage: create root %q: %w", abs, err)
	}
	return &Local{root: abs}, nil
}

// Root returns the absolute directory the store writes into.
func (l *Local) Root() string { return l.root }

// Put writes r to key, replacing any existing object, and reports the
// number of bytes stored.
func (l *Local) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	path, err := l.resolve(key)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("storage: put %q: %w", key, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return 0, fmt.Errorf("storage: create directory for %q: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return 0, fmt.Errorf("storage: create temp file for %q: %w", key, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: both calls are no-ops once the happy path has
	// closed and renamed the file.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	n, err := io.Copy(tmp, r)
	if err != nil {
		return 0, fmt.Errorf("storage: write %q: %w", key, err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("storage: sync %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("storage: close %q: %w", key, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return 0, fmt.Errorf("storage: commit %q: %w", key, err)
	}
	return n, nil
}

// Get opens the object at key. The caller closes the returned reader.
func (l *Local) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	path, err := l.resolve(key)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("storage: get %q: %w", key, err)
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("storage: get %q: %w", key, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("storage: open %q: %w", key, err)
	}
	return f, nil
}

// Delete removes the object at key.
func (l *Local) Delete(ctx context.Context, key string) error {
	path, err := l.resolve(key)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("storage: delete %q: %w", key, ErrNotFound)
		}
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	return nil
}

// resolve maps a store key onto an absolute path inside the root.
func (l *Local) resolve(key string) (string, error) {
	clean, err := validateKey(key)
	if err != nil {
		return "", err
	}
	path := filepath.Join(l.root, filepath.FromSlash(clean))
	if path != l.root && !strings.HasPrefix(path, l.root+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: key %q escapes the store root", key)
	}
	return path, nil
}

// Local satisfies BlobStore.
var _ BlobStore = (*Local)(nil)
