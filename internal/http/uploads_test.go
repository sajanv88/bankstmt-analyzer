package http_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
)

// testFile is one part of a multipart upload body.
type testFile struct {
	field   string
	name    string
	content []byte
}

func pdf(size int) []byte {
	body := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), max(0, size-9))...)
	return body
}

// multipartRequest builds a POST /api/v1/uploads request carrying files.
func multipartRequest(t *testing.T, files []testFile) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, f := range files {
		part, err := writer.CreateFormFile(f.field, f.name)
		require.NoError(t, err)
		_, err = part.Write(f.content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/uploads", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestCreateUploadAcceptsPDFs(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	rec := server.do(t, multipartRequest(t, []testFile{
		{field: "files", name: "january.pdf", content: pdf(64)},
		{field: "files", name: "february.pdf", content: pdf(128)},
	}))

	require.Equal(t, http.StatusAccepted, rec.Code)

	var resp apihttp.CreateUploadResponse
	require.NoError(t, decodeJSON(t, rec, &resp))
	assert.Equal(t, db.StatusPending, resp.Status)
	uploadID, err := uuid.Parse(resp.ID)
	require.NoError(t, err, "the response id should be a uuid")

	created := server.store.createdUploads()
	require.Len(t, created, 1)
	assert.Equal(t, uploadID, created[0].ID)
	require.Len(t, created[0].Files, 2)

	// Position must follow submission order, since that is what orders the
	// statements presented to the model.
	assert.Equal(t, "january.pdf", created[0].Files[0].Filename)
	assert.Equal(t, int32(0), created[0].Files[0].Position)
	assert.Equal(t, "february.pdf", created[0].Files[1].Filename)
	assert.Equal(t, int32(1), created[0].Files[1].Position)
	assert.Equal(t, int64(64), created[0].Files[0].SizeBytes)
	assert.Equal(t, int64(128), created[0].Files[1].SizeBytes)

	assert.Len(t, server.blobs.keys(), 2, "both PDFs should be in the blob store")
	assert.Equal(t, []uuid.UUID{uploadID}, server.enqueuer.enqueued())
}

// TestCreateUploadStoresUnderOpaqueKeys proves a crafted filename cannot
// influence where the bytes land.
func TestCreateUploadStoresUnderOpaqueKeys(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	rec := server.do(t, multipartRequest(t, []testFile{
		{field: "files", name: "../../etc/passwd.pdf", content: pdf(32)},
	}))

	require.Equal(t, http.StatusAccepted, rec.Code)

	created := server.store.createdUploads()
	require.Len(t, created, 1)
	require.Len(t, created[0].Files, 1)
	assert.Equal(t, "passwd.pdf", created[0].Files[0].Filename, "the name should be reduced to its final element")

	keys := server.blobs.keys()
	require.Len(t, keys, 1)
	assert.NotContains(t, keys[0], "..")
	assert.NotContains(t, keys[0], "passwd")
	assert.True(t, strings.HasPrefix(keys[0], "uploads/"), "keys are built from uuids, got %q", keys[0])
}

func TestCreateUploadRejectsBadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		files      []testFile
		wantStatus int
		wantDetail string
	}{
		{
			name:       "no files at all",
			files:      nil,
			wantStatus: http.StatusBadRequest,
			wantDetail: "At least one PDF",
		},
		{
			name:       "the field name is wrong",
			files:      []testFile{{field: "statements", name: "a.pdf", content: pdf(32)}},
			wantStatus: http.StatusBadRequest,
			wantDetail: "At least one PDF",
		},
		{
			name:       "a file is not a PDF",
			files:      []testFile{{field: "files", name: "a.pdf", content: []byte("PK\x03\x04 not a pdf")}},
			wantStatus: http.StatusBadRequest,
			wantDetail: "must be a PDF",
		},
		{
			name:       "an empty file",
			files:      []testFile{{field: "files", name: "a.pdf", content: nil}},
			wantStatus: http.StatusBadRequest,
			wantDetail: "must be a PDF",
		},
		{
			name: "one good file and one bad one",
			files: []testFile{
				{field: "files", name: "good.pdf", content: pdf(32)},
				{field: "files", name: "bad.pdf", content: []byte("not a pdf at all")},
			},
			wantStatus: http.StatusBadRequest,
			wantDetail: "must be a PDF",
		},
		{
			name: "more files than allowed",
			files: []testFile{
				{field: "files", name: "a.pdf", content: pdf(32)},
				{field: "files", name: "b.pdf", content: pdf(32)},
				{field: "files", name: "c.pdf", content: pdf(32)},
				{field: "files", name: "d.pdf", content: pdf(32)},
			},
			wantStatus: http.StatusBadRequest,
			wantDetail: "At most 3 files",
		},
		{
			name:       "a file larger than the per-file limit",
			files:      []testFile{{field: "files", name: "big.pdf", content: pdf(2048)}},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantDetail: "exceeds the 1024 byte limit",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t)

			rec := server.do(t, multipartRequest(t, tc.files))

			require.Equal(t, tc.wantStatus, rec.Code)
			problem := decodeProblem(t, rec)
			assert.Contains(t, problem.Detail, tc.wantDetail)

			assert.Empty(t, server.store.createdUploads(), "nothing should be recorded for a rejected upload")
			assert.Empty(t, server.blobs.keys(), "a rejected upload must leave no blobs behind")
			assert.Zero(t, server.enqueuer.called, "a rejected upload must not be queued")
		})
	}
}

func TestCreateUploadRejectsNonMultipart(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/uploads",
		strings.NewReader(`{"files":[]}`))
	req.Header.Set("Content-Type", "application/json")

	rec := server.do(t, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, decodeProblem(t, rec).Detail, "multipart/form-data")
}

// TestCreateUploadCleansUpWhenPersistenceFails proves stored blobs are not
// orphaned when the database rejects the upload.
func TestCreateUploadCleansUpWhenPersistenceFails(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.store.createErr = errors.New("connection reset")

	rec := server.do(t, multipartRequest(t, []testFile{
		{field: "files", name: "a.pdf", content: pdf(32)},
		{field: "files", name: "b.pdf", content: pdf(32)},
	}))

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	problem := decodeProblem(t, rec)
	assert.NotContains(t, problem.Detail, "connection reset", "internal errors must not reach the client")

	assert.Empty(t, server.blobs.keys(), "blobs must be removed when the upload is not recorded")
	assert.Zero(t, server.enqueuer.called)
}

// TestCreateUploadMarksFailedWhenEnqueueFails covers the one window where
// an upload can exist with no queued work: it must not be left pending.
func TestCreateUploadMarksFailedWhenEnqueueFails(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	server.enqueuer.err = errors.New("broker unreachable at postgres://user:hunter2@db:5432")

	rec := server.do(t, multipartRequest(t, []testFile{
		{field: "files", name: "a.pdf", content: pdf(32)},
	}))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	problem := decodeProblem(t, rec)
	assert.NotContains(t, problem.Detail, "hunter2", "credentials must never reach the client")

	updates := server.store.statusUpdates()
	require.Len(t, updates, 1)
	assert.Equal(t, db.StatusFailed, updates[0].Status)
	require.NotNil(t, updates[0].FailureReason)
	assert.Contains(t, *updates[0].FailureReason, "enqueue:")
	assert.NotContains(t, *updates[0].FailureReason, "hunter2", "credentials must never be persisted")
}

func TestUploadStatus(t *testing.T) {
	t.Parallel()

	created := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2025, 3, 1, 10, 5, 0, 0, time.UTC)
	reason := "ocr: the document could not be read"

	tests := []struct {
		name           string
		seed           func(*fakeStore) uuid.UUID
		wantStatus     int
		wantBodyStatus string
		wantReason     string
		wantAnalysis   bool
	}{
		{
			name: "pending",
			seed: func(s *fakeStore) uuid.UUID {
				id := uuid.New()
				s.putUpload(db.Upload{ID: id, Status: db.StatusPending, FileCount: 1,
					CreatedAt: timestamp(created), UpdatedAt: timestamp(updated)})
				return id
			},
			wantStatus:     http.StatusOK,
			wantBodyStatus: db.StatusPending,
		},
		{
			name: "failed carries the reason",
			seed: func(s *fakeStore) uuid.UUID {
				id := uuid.New()
				s.putUpload(db.Upload{ID: id, Status: db.StatusFailed, FailureReason: &reason,
					CreatedAt: timestamp(created), UpdatedAt: timestamp(updated)})
				return id
			},
			wantStatus:     http.StatusOK,
			wantBodyStatus: db.StatusFailed,
			wantReason:     reason,
		},
		{
			name: "completed carries the analysis id",
			seed: func(s *fakeStore) uuid.UUID {
				id := uuid.New()
				s.putUpload(db.Upload{ID: id, Status: db.StatusCompleted,
					CreatedAt: timestamp(created), UpdatedAt: timestamp(updated)})
				s.putAnalysis(id, db.Analysis{ID: uuid.New(), UploadID: id, Currency: "EUR"})
				return id
			},
			wantStatus:     http.StatusOK,
			wantBodyStatus: db.StatusCompleted,
			wantAnalysis:   true,
		},
		{
			name: "unknown upload",
			seed: func(*fakeStore) uuid.UUID {
				return uuid.New()
			},
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t)
			id := tc.seed(server.store)

			rec := server.do(t, httptest.NewRequestWithContext(t.Context(),
				http.MethodGet, "/api/v1/uploads/"+id.String()+"/status", nil))

			require.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus != http.StatusOK {
				decodeProblem(t, rec)
				return
			}

			var resp apihttp.UploadStatusResponse
			require.NoError(t, decodeJSON(t, rec, &resp))
			assert.Equal(t, id.String(), resp.ID)
			assert.Equal(t, tc.wantBodyStatus, resp.Status)
			assert.Equal(t, tc.wantReason, resp.FailureReason)
			assert.True(t, created.Equal(resp.CreatedAt))
			assert.True(t, updated.Equal(resp.UpdatedAt))

			if tc.wantAnalysis {
				assert.NotEmpty(t, resp.AnalysisID)
			} else {
				assert.Empty(t, resp.AnalysisID, "analysis_id belongs only to a completed upload")
			}
		})
	}
}

// TestUploadStatusOmitsAbsentFields pins the wire format: a pending upload
// must carry neither failure_reason nor analysis_id at all.
func TestUploadStatusOmitsAbsentFields(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	id := uuid.New()
	server.store.putUpload(db.Upload{ID: id, Status: db.StatusPending})

	rec := server.do(t, httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/uploads/"+id.String()+"/status", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "failure_reason")
	assert.NotContains(t, body, "analysis_id")
}

func TestUploadStatusRejectsMalformedID(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	rec := server.do(t, httptest.NewRequestWithContext(t.Context(),
		http.MethodGet, "/api/v1/uploads/not-a-uuid/status", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	problem := decodeProblem(t, rec)
	require.Len(t, problem.InvalidParams, 1)
	assert.Equal(t, "id", problem.InvalidParams[0].Name)
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// decodeJSON decodes a successful response body.
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, into any) error {
	t.Helper()
	require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	return json.NewDecoder(io.NopCloser(bytes.NewReader(rec.Body.Bytes()))).Decode(into)
}
