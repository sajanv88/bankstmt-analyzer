package storage_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// One contract, run against every implementation.
//
// The two backends are interchangeable only if they behave identically, and
// the ways they can drift apart are exactly the interesting ones: what
// counts as a valid key, whether deleting a missing object is an error,
// whether a second Put replaces or appends. Testing them separately would
// let those diverge silently, so the suite is written once and applied to
// both — the local store always, and the S3 store whenever S3_ENDPOINT and
// S3_BUCKET point at a MinIO the test may write to.

// newStore builds a store for one conformance run, or skips the test when
// the backend is not configured.
type newStore func(t *testing.T) storage.BlobStore

func backends() []struct {
	name string
	new  newStore
} {
	return []struct {
		name string
		new  newStore
	}{
		{name: "local", new: newLocalStore},
		{name: "s3", new: newS3Store},
	}
}

func newLocalStore(t *testing.T) storage.BlobStore {
	t.Helper()
	store, err := storage.NewLocal(t.TempDir())
	require.NoError(t, err)
	return store
}

// newS3Store points at a MinIO (or any S3-compatible store) when one is
// configured, and gives every run its own key prefix so parallel tests
// cannot collide inside a shared bucket.
func newS3Store(t *testing.T) storage.BlobStore {
	t.Helper()

	endpoint := os.Getenv("S3_ENDPOINT")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("S3_ENDPOINT and S3_BUCKET are not set; skipping the object store conformance run")
	}

	store, err := storage.NewS3(t.Context(), config.S3Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Region:          envOr("S3_REGION", "us-east-1"),
		AccessKeyID:     os.Getenv("S3_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("S3_SECRET_ACCESS_KEY"),
		UsePathStyle:    true,
		Prefix:          "conformance/" + uuid.NewString(),
		Timeout:         30 * time.Second,
	})
	require.NoError(t, err)
	return store
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func TestBlobStoreRoundTrip(t *testing.T) {
	t.Parallel()

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)
			ctx := t.Context()

			const key = "uploads/abc/def.pdf"
			n, err := store.Put(ctx, key, strings.NewReader("%PDF-1.7 body"))
			require.NoError(t, err)
			assert.Equal(t, int64(13), n, "Put should report the bytes written")

			rc, err := store.Get(ctx, key)
			require.NoError(t, err)
			body, err := io.ReadAll(rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			assert.Equal(t, "%PDF-1.7 body", string(body))

			require.NoError(t, store.Delete(ctx, key))

			_, err = store.Get(ctx, key)
			assert.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestBlobStorePutOverwrites(t *testing.T) {
	t.Parallel()

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)
			ctx := t.Context()

			const key = "uploads/one.pdf"
			_, err := store.Put(ctx, key, strings.NewReader("first"))
			require.NoError(t, err)
			_, err = store.Put(ctx, key, strings.NewReader("second"))
			require.NoError(t, err)

			rc, err := store.Get(ctx, key)
			require.NoError(t, err)
			body, err := io.ReadAll(rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			assert.Equal(t, "second", string(body), "a second Put replaces rather than appends")
		})
	}
}

// TestBlobStoreMissingObject pins the behaviour the upload handler relies
// on when cleaning up after a rejected upload.
func TestBlobStoreMissingObject(t *testing.T) {
	t.Parallel()

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)
			ctx := t.Context()

			_, err := store.Get(ctx, "uploads/never-written.pdf")
			assert.ErrorIs(t, err, storage.ErrNotFound)

			// S3 would happily "delete" a key that was never there; the
			// contract says otherwise, and the handler distinguishes the
			// two when deciding whether to log a warning.
			err = store.Delete(ctx, "uploads/never-written.pdf")
			assert.ErrorIs(t, err, storage.ErrNotFound)
		})
	}
}

func TestBlobStoreRejectsEscapingKeys(t *testing.T) {
	t.Parallel()

	keys := []struct {
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

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)
			ctx := t.Context()

			for _, tc := range keys {
				t.Run(tc.name, func(t *testing.T) {
					// Rejection must be a validation failure, not a
					// "missing object": the handler treats ErrNotFound as
					// benign, so conflating them would hide a bad key.
					_, err := store.Put(ctx, tc.key, strings.NewReader("x"))
					require.Error(t, err)
					assert.NotErrorIs(t, err, storage.ErrNotFound)

					_, err = store.Get(ctx, tc.key)
					require.Error(t, err)
					assert.NotErrorIs(t, err, storage.ErrNotFound)

					err = store.Delete(ctx, tc.key)
					require.Error(t, err)
					assert.NotErrorIs(t, err, storage.ErrNotFound)
				})
			}
		})
	}
}

// TestBlobStoreLargeObject exercises the multipart path of the S3 uploader,
// which a small object never reaches.
func TestBlobStoreLargeObject(t *testing.T) {
	t.Parallel()

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)
			ctx := t.Context()

			// Above the uploader's 5 MiB default part size, so the object
			// is sent in more than one part.
			const size = 6 << 20
			body := strings.Repeat("x", size)

			n, err := store.Put(ctx, "uploads/large.pdf", strings.NewReader(body))
			require.NoError(t, err)
			assert.Equal(t, int64(size), n)

			rc, err := store.Get(ctx, "uploads/large.pdf")
			require.NoError(t, err)
			read, err := io.Copy(io.Discard, rc)
			require.NoError(t, err)
			require.NoError(t, rc.Close())
			assert.Equal(t, int64(size), read, "the object should come back whole")
		})
	}
}

func TestBlobStoreContextCancellation(t *testing.T) {
	t.Parallel()

	for _, backend := range backends() {
		t.Run(backend.name, func(t *testing.T) {
			t.Parallel()
			store := backend.new(t)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			_, err := store.Put(ctx, "uploads/cancelled.pdf", strings.NewReader("x"))
			require.Error(t, err)
			assert.True(t, errors.Is(err, context.Canceled),
				"a cancelled context should surface as context.Canceled, got %v", err)
		})
	}
}
