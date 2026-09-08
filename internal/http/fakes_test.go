package http_test

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// fakeStore is an in-memory UploadStore. Aggregation results are canned
// rather than computed: the SQL that produces them is exercised against a
// real Postgres in the integration test, and what these handler tests care
// about is the window the handler asks for and how it shapes the response.
type fakeStore struct {
	mu sync.Mutex

	uploads  map[uuid.UUID]db.Upload
	analyses map[uuid.UUID]db.Analysis // keyed by upload id

	// Injected failures.
	createErr    error
	getUploadErr error

	// Recorded calls.
	created      []createdUpload
	statusWrites []db.SetUploadStatusParams
	windowAsked  []db.WindowSummaryParams

	// Canned aggregation results.
	bounds          db.GetTransactionMonthBoundsRow
	income          []db.MonthlyIncomeVsSpendingRow
	categories      []db.CategoryBreakdownRow
	monthCategories []db.MonthlyByCategoryRow
	splits          []db.EssentialVsDiscretionaryRow
	balances        []db.BalanceOverTimeRow
	summary         db.WindowSummaryRow
}

type createdUpload struct {
	ID    uuid.UUID
	Files []db.NewUploadFile
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		uploads:  map[uuid.UUID]db.Upload{},
		analyses: map[uuid.UUID]db.Analysis{},
	}
}

func (f *fakeStore) putUpload(u db.Upload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[u.ID] = u
}

func (f *fakeStore) putAnalysis(uploadID uuid.UUID, a db.Analysis) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.analyses[uploadID] = a
}

func (f *fakeStore) createdUploads() []createdUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]createdUpload(nil), f.created...)
}

func (f *fakeStore) statusUpdates() []db.SetUploadStatusParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.SetUploadStatusParams(nil), f.statusWrites...)
}

func (f *fakeStore) windowsRequested() []db.WindowSummaryParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]db.WindowSummaryParams(nil), f.windowAsked...)
}

func (f *fakeStore) CreateUploadWithFiles(_ context.Context, uploadID uuid.UUID, files []db.NewUploadFile) (db.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return db.Upload{}, f.createErr
	}
	f.created = append(f.created, createdUpload{ID: uploadID, Files: files})
	upload := db.Upload{ID: uploadID, Status: db.StatusPending, FileCount: int32(len(files))}
	f.uploads[uploadID] = upload
	return upload, nil
}

func (f *fakeStore) GetUpload(_ context.Context, id uuid.UUID) (db.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getUploadErr != nil {
		return db.Upload{}, f.getUploadErr
	}
	upload, ok := f.uploads[id]
	if !ok {
		return db.Upload{}, pgx.ErrNoRows
	}
	return upload, nil
}

func (f *fakeStore) SetUploadStatus(_ context.Context, arg db.SetUploadStatusParams) (db.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func (f *fakeStore) GetAnalysisByUpload(_ context.Context, uploadID uuid.UUID) (db.Analysis, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	analysis, ok := f.analyses[uploadID]
	if !ok {
		return db.Analysis{}, pgx.ErrNoRows
	}
	return analysis, nil
}

func (f *fakeStore) GetAnalysisIDByUpload(ctx context.Context, uploadID uuid.UUID) (uuid.UUID, error) {
	analysis, err := f.GetAnalysisByUpload(ctx, uploadID)
	if err != nil {
		return uuid.Nil, err
	}
	return analysis.ID, nil
}

func (f *fakeStore) GetTransactionMonthBounds(context.Context, uuid.UUID) (db.GetTransactionMonthBoundsRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bounds, nil
}

func (f *fakeStore) MonthlyIncomeVsSpending(_ context.Context, _ db.MonthlyIncomeVsSpendingParams) ([]db.MonthlyIncomeVsSpendingRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.income, nil
}

func (f *fakeStore) CategoryBreakdown(_ context.Context, _ db.CategoryBreakdownParams) ([]db.CategoryBreakdownRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.categories, nil
}

func (f *fakeStore) MonthlyByCategory(_ context.Context, _ db.MonthlyByCategoryParams) ([]db.MonthlyByCategoryRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.monthCategories, nil
}

func (f *fakeStore) EssentialVsDiscretionary(_ context.Context, _ db.EssentialVsDiscretionaryParams) ([]db.EssentialVsDiscretionaryRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.splits, nil
}

func (f *fakeStore) BalanceOverTime(_ context.Context, _ db.BalanceOverTimeParams) ([]db.BalanceOverTimeRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balances, nil
}

func (f *fakeStore) WindowSummary(_ context.Context, arg db.WindowSummaryParams) (db.WindowSummaryRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.windowAsked = append(f.windowAsked, arg)
	return f.summary, nil
}

// fakeBlobs is an in-memory BlobStore.
type fakeBlobs struct {
	mu      sync.Mutex
	objects map[string][]byte
	putErr  error
}

func newFakeBlobs() *fakeBlobs {
	return &fakeBlobs{objects: map[string][]byte{}}
}

func (f *fakeBlobs) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	return keys
}

func (f *fakeBlobs) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	// Read outside the lock: Put is the slow path and the handler streams
	// into it one file at a time.
	body, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return 0, f.putErr
	}
	f.objects[key] = body
	return int64(len(body)), nil
}

func (f *fakeBlobs) Get(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.objects[key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (f *fakeBlobs) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.objects[key]; !ok {
		return storage.ErrNotFound
	}
	delete(f.objects, key)
	return nil
}

// fakeEnqueuer records the uploads handed to the pipeline.
type fakeEnqueuer struct {
	mu     sync.Mutex
	ids    []uuid.UUID
	err    error
	called int
}

func (f *fakeEnqueuer) Enqueue(_ context.Context, uploadID uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called++
	if f.err != nil {
		return f.err
	}
	f.ids = append(f.ids, uploadID)
	return nil
}

func (f *fakeEnqueuer) enqueued() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.ids...)
}
