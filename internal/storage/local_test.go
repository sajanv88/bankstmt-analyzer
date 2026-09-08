package storage_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// Behaviour every backend shares is covered once, in conformance_test.go.
// What remains here is specific to writing files on a disk.

// TestLocalPutLeavesNoPartials proves the temp-file-and-rename write leaves
// nothing behind when the source reader fails midway, so a crashed upload
// cannot leave a truncated PDF that later looks like a whole one.
func TestLocalPutLeavesNoPartials(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := storage.NewLocal(root)
	require.NoError(t, err)

	_, err = store.Put(t.Context(), "uploads/broken.pdf", failingReader{})
	require.Error(t, err)

	_, err = store.Get(t.Context(), "uploads/broken.pdf")
	assert.ErrorIs(t, err, storage.ErrNotFound)

	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	require.NoError(t, err)
	assert.Empty(t, entries, "the aborted write should leave no temp file behind")
}

func TestNewLocalRejectsEmptyRoot(t *testing.T) {
	t.Parallel()

	_, err := storage.NewLocal("   ")
	require.Error(t, err)
}

func TestNewLocalCreatesItsRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "nested", "uploads")
	store, err := storage.NewLocal(root)
	require.NoError(t, err)

	info, err := os.Stat(store.Root())
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
