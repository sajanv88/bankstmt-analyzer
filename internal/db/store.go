package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the generated query set plus the pool it runs against, so a
// caller that needs several statements to land together can get a
// transaction without reaching for the pool itself.
type Store struct {
	*Queries
	pool *pgxpool.Pool
}

// NewStore binds the generated queries to pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Queries: New(pool), pool: pool}
}

// Ping reports whether the database is reachable. It is what backs the
// readiness probe.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("db: ping: %w", err)
	}
	return nil
}

// InTx runs fn inside one transaction, committing if it returns nil and
// rolling back otherwise. fn receives a *Queries bound to the transaction;
// using the outer Store inside fn would silently run outside it.
func (s *Store) InTx(ctx context.Context, fn func(*Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op that returns
	// pgx.ErrTxClosed, so this needs no committed flag to guard it.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(s.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit transaction: %w", err)
	}
	return nil
}

// NewUploadFile is one file to record against a new upload. The bytes are
// already in the BlobStore by the time this is used; StorageKey is where.
type NewUploadFile struct {
	ID          uuid.UUID
	Position    int32
	Filename    string
	ContentType string
	SizeBytes   int64
	StorageKey  string
}

// CreateUploadWithFiles inserts the upload and all of its files in one
// transaction, so a partially recorded upload can never become visible to
// the status endpoint or to the pipeline.
func (s *Store) CreateUploadWithFiles(ctx context.Context, uploadID uuid.UUID, files []NewUploadFile) (Upload, error) {
	if len(files) == 0 {
		return Upload{}, errors.New("db: an upload needs at least one file")
	}

	var upload Upload
	err := s.InTx(ctx, func(q *Queries) error {
		var err error
		upload, err = q.CreateUpload(ctx, CreateUploadParams{
			ID:        uploadID,
			Status:    StatusPending,
			FileCount: int32(len(files)),
		})
		if err != nil {
			return fmt.Errorf("db: insert upload: %w", err)
		}
		for _, f := range files {
			if _, err := q.CreateUploadFile(ctx, CreateUploadFileParams{
				ID:          f.ID,
				UploadID:    uploadID,
				Position:    f.Position,
				Filename:    f.Filename,
				ContentType: f.ContentType,
				SizeBytes:   f.SizeBytes,
				StorageKey:  f.StorageKey,
			}); err != nil {
				return fmt.Errorf("db: insert upload file %q: %w", f.Filename, err)
			}
		}
		return nil
	})
	if err != nil {
		return Upload{}, err
	}
	return upload, nil
}

// IsNotFound reports whether err is pgx's "query returned no rows", which
// every :one query produces for a missing row.
func IsNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// NewAnalysis is one complete analysis ready to be written: the analysis
// row itself and the transactions flattened out of it.
type NewAnalysis struct {
	Analysis     CreateAnalysisParams
	Transactions []CreateTransactionsParams
}

// ReplaceAnalysis writes an upload's analysis and its transactions in a
// single transaction, discarding whatever analysis was recorded for that
// upload before.
//
// Replacing rather than inserting is what makes the analyze step safe to
// re-run. taskQ delivers at least once, so the step can execute twice for
// one upload; a plain insert would either violate the unique constraint on
// upload_id or double every transaction row.
func (s *Store) ReplaceAnalysis(ctx context.Context, in NewAnalysis) (Analysis, error) {
	var analysis Analysis

	err := s.InTx(ctx, func(q *Queries) error {
		if _, err := q.DeleteAnalysisByUpload(ctx, in.Analysis.UploadID); err != nil {
			return fmt.Errorf("db: delete previous analysis: %w", err)
		}

		var err error
		analysis, err = q.CreateAnalysis(ctx, in.Analysis)
		if err != nil {
			return fmt.Errorf("db: insert analysis: %w", err)
		}
		if len(in.Transactions) == 0 {
			return nil
		}

		// The analysis id is only known now, so it is stamped onto the
		// rows here rather than being the caller's problem.
		rows := make([]CreateTransactionsParams, len(in.Transactions))
		for i, txn := range in.Transactions {
			txn.AnalysisID = analysis.ID
			rows[i] = txn
		}
		copied, err := q.CreateTransactions(ctx, rows)
		if err != nil {
			return fmt.Errorf("db: copy transactions: %w", err)
		}
		if copied != int64(len(rows)) {
			return fmt.Errorf("db: copied %d of %d transactions", copied, len(rows))
		}
		return nil
	})
	if err != nil {
		return Analysis{}, err
	}
	return analysis, nil
}
