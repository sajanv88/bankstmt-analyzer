package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
	"github.com/sajanv88/bankstmt-analyzer/internal/ocr"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// fakeStore is an in-memory pipeline.Store.
type fakeStore struct {
	mu sync.Mutex

	uploads  map[uuid.UUID]db.Upload
	files    map[uuid.UUID][]db.UploadFile // keyed by upload id
	analyses map[uuid.UUID]db.Analysis     // keyed by upload id

	// Injected failures.
	setStatusErr  map[string]error // keyed by the status being written
	replaceErr    error
	deleteErr     error
	setFileOCRErr error

	// Recorded calls.
	statusWrites []db.SetUploadStatusParams
	replaced     []db.NewAnalysis
	deleteCalls  int
	ocrWrites    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		uploads:      map[uuid.UUID]db.Upload{},
		files:        map[uuid.UUID][]db.UploadFile{},
		analyses:     map[uuid.UUID]db.Analysis{},
		setStatusErr: map[string]error{},
	}
}

func (f *fakeStore) seedUpload(uploadID uuid.UUID, files []db.UploadFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[uploadID] = db.Upload{ID: uploadID, Status: db.StatusPending, FileCount: int32(len(files))}
	f.files[uploadID] = files
}

func (f *fakeStore) upload(uploadID uuid.UUID) (db.Upload, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	upload, ok := f.uploads[uploadID]
	return upload, ok
}

func (f *fakeStore) analysisFor(uploadID uuid.UUID) (db.Analysis, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	analysis, ok := f.analyses[uploadID]
	return analysis, ok
}

func (f *fakeStore) storedAnalyses() []db.NewAnalysis {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.NewAnalysis(nil), f.replaced...)
}

func (f *fakeStore) deletions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteCalls
}

func (f *fakeStore) ocrWriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ocrWrites
}

func (f *fakeStore) SetUploadStatus(_ context.Context, arg db.SetUploadStatusParams) (db.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.setStatusErr[arg.Status]; err != nil {
		return db.Upload{}, err
	}
	f.statusWrites = append(f.statusWrites, arg)

	upload, ok := f.uploads[arg.ID]
	if !ok {
		return db.Upload{}, pgx.ErrNoRows
	}
	upload.Status = arg.Status
	upload.FailureReason = arg.FailureReason
	f.uploads[arg.ID] = upload
	return upload, nil
}

func (f *fakeStore) ListUploadFiles(_ context.Context, uploadID uuid.UUID) ([]db.UploadFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.UploadFile(nil), f.files[uploadID]...), nil
}

func (f *fakeStore) SetUploadFileOCR(_ context.Context, arg db.SetUploadFileOCRParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setFileOCRErr != nil {
		return f.setFileOCRErr
	}
	f.ocrWrites++
	for uploadID, files := range f.files {
		for i := range files {
			if files[i].ID == arg.ID {
				files[i].OcrMarkdown = arg.OcrMarkdown
				files[i].PageCount = arg.PageCount
				f.files[uploadID] = files
				return nil
			}
		}
	}
	return pgx.ErrNoRows
}

func (f *fakeStore) ReplaceAnalysis(_ context.Context, in db.NewAnalysis) (db.Analysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replaceErr != nil {
		return db.Analysis{}, f.replaceErr
	}
	f.replaced = append(f.replaced, in)
	analysis := db.Analysis{
		ID:       in.Analysis.ID,
		UploadID: in.Analysis.UploadID,
		Currency: in.Analysis.Currency,
	}
	f.analyses[in.Analysis.UploadID] = analysis
	return analysis, nil
}

func (f *fakeStore) DeleteAnalysisByUpload(_ context.Context, uploadID uuid.UUID) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteErr != nil {
		return 0, f.deleteErr
	}
	if _, ok := f.analyses[uploadID]; !ok {
		return 0, nil
	}
	delete(f.analyses, uploadID)
	return 1, nil
}

// fakeBlobs serves canned PDF bytes.
type fakeBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	getErr  error
}

func newFakeBlobs() *fakeBlobs { return &fakeBlobs{objects: map[string][]byte{}} }

func (f *fakeBlobs) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = body
}

func (f *fakeBlobs) Put(context.Context, string, io.Reader) (int64, error) { return 0, nil }

func (f *fakeBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeBlobs) Delete(context.Context, string) error { return nil }

// fakeOCR returns canned pages, or an error.
type fakeOCR struct {
	mu     sync.Mutex
	result ocr.Result
	err    error
	calls  int
}

func (f *fakeOCR) Extract(context.Context, []byte) (ocr.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return ocr.Result{}, f.err
	}
	return f.result, nil
}

func (f *fakeOCR) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeLLM returns a canned analysis, or an error.
type fakeLLM struct {
	mu         sync.Mutex
	response   *llm.Response
	err        error
	calls      int
	statements []llm.Statement
}

func (f *fakeLLM) Analyze(_ context.Context, statements []llm.Statement) (*llm.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.statements = append([]llm.Statement(nil), statements...)
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeLLM) lastStatements() []llm.Statement {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.Statement(nil), f.statements...)
}
