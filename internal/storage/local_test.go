package storage_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

func TestLocalRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)

	const key = "uploads/abc/def.pdf"
	n, err := store.Put(ctx, key, strings.NewReader("%PDF-1.7 body"))
	require.NoError(t, err)
	assert.Equal(t, int64(13), n)

	rc, err := store.Get(ctx, key)
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	// Closed before Delete rather than in a Cleanup: Windows refuses to
	// remove a file that still has an open handle.
	require.NoError(t, rc.Close())
	assert.Equal(t, "%PDF-1.7 body", string(body))

	require.NoError(t, store.Delete(ctx, key))
	_, err = store.Get(ctx, key)
	assert.ErrorIs(t, err, storage.ErrNotFound)
}

func TestLocalPutOverwrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)

	const key = "uploads/one.pdf"
	_, err = store.Put(ctx, key, strings.NewReader("first"))
	require.NoError(t, err)
	_, err = store.Put(ctx, key, strings.NewReader("second"))
	require.NoError(t, err)

	rc, err := store.Get(ctx, key)
	require.NoError(t, err)
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Equal(t, "second", string(body))
}

// TestLocalPutLeavesNoPartials proves the temp-file-and-rename write leaves
// nothing behind when the source reader fails midway.
func TestLocalPutLeavesNoPartials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.NewLocal(root)
	require.NoError(t, err)

	_, err = store.Put(ctx, "uploads/broken.pdf", failingReader{})
	require.Error(t, err)

	_, err = store.Get(ctx, "uploads/broken.pdf")
	assert.ErrorIs(t, err, storage.ErrNotFound)

	entries, err := os.ReadDir(filepath.Join(root, "uploads"))
	require.NoError(t, err)
	assert.Empty(t, entries, "the aborted write should leave no temp file behind")
}

func TestLocalRejectsEscapingKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	store, err := storage.NewLocal(root)
	require.NoError(t, err)

	tests := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "whitespace only", key: "   "},
		{name: "parent traversal", key: "../escaped.pdf"},
		{name: "nested traversal", key: "uploads/../../escaped.pdf"},
		{name: "absolute unix path", key: "/etc/passwd"},
		{name: "backslash traversal", key: `..\escaped.pdf`},
		{name: "leading backslash", key: `\etc\passwd`},
		{name: "windows volume", key: `C:\Windows\system32`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := store.Put(ctx, tc.key, strings.NewReader("x"))
			require.Error(t, err)
			assert.NotErrorIs(t, err, storage.ErrNotFound)

			_, err = store.Get(ctx, tc.key)
			require.Error(t, err)
			assert.NotErrorIs(t, err, storage.ErrNotFound)

			require.Error(t, store.Delete(ctx, tc.key))
		})
	}
}

func TestNewLocalRejectsEmptyRoot(t *testing.T) {
	t.Parallel()
	_, err := storage.NewLocal("   ")
	require.Error(t, err)
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
