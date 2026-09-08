package http

import (
	"context"

	"github.com/google/uuid"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
)

// UploadStore is the persistence the handlers depend on. It is declared
// here, at the point of use, rather than exported from the db package, so
// the handler tests can supply a fake without a database. *db.Store
// satisfies it.
type UploadStore interface {
	CreateUploadWithFiles(ctx context.Context, uploadID uuid.UUID, files []db.NewUploadFile) (db.Upload, error)
	GetUpload(ctx context.Context, id uuid.UUID) (db.Upload, error)
	SetUploadStatus(ctx context.Context, arg db.SetUploadStatusParams) (db.Upload, error)
	GetAnalysisByUpload(ctx context.Context, uploadID uuid.UUID) (db.Analysis, error)
	GetAnalysisIDByUpload(ctx context.Context, uploadID uuid.UUID) (uuid.UUID, error)

	GetTransactionMonthBounds(ctx context.Context, analysisID uuid.UUID) (db.GetTransactionMonthBoundsRow, error)
	MonthlyIncomeVsSpending(ctx context.Context, arg db.MonthlyIncomeVsSpendingParams) ([]db.MonthlyIncomeVsSpendingRow, error)
	CategoryBreakdown(ctx context.Context, arg db.CategoryBreakdownParams) ([]db.CategoryBreakdownRow, error)
	MonthlyByCategory(ctx context.Context, arg db.MonthlyByCategoryParams) ([]db.MonthlyByCategoryRow, error)
	EssentialVsDiscretionary(ctx context.Context, arg db.EssentialVsDiscretionaryParams) ([]db.EssentialVsDiscretionaryRow, error)
	BalanceOverTime(ctx context.Context, arg db.BalanceOverTimeParams) ([]db.BalanceOverTimeRow, error)
	WindowSummary(ctx context.Context, arg db.WindowSummaryParams) (db.WindowSummaryRow, error)
}

// Enqueuer starts the analysis pipeline for an upload that has already been
// persisted. internal/pipeline implements it over taskQ's saga; the handler
// neither knows nor cares which queue backend is behind it.
type Enqueuer interface {
	Enqueue(ctx context.Context, uploadID uuid.UUID) error
}
