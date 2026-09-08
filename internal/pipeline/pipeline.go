// Package pipeline runs the analysis workflow: OCR each uploaded
// statement, hand the extracted markdown to the model, and store what
// comes back.
//
// The workflow is a taskQ saga over the Postgres broker. Each step is one
// hop through the queue, so a worker restart resumes wherever the run left
// off rather than starting again, and a step that fails after earlier ones
// succeeded unwinds them through their compensations.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	taskq "github.com/Nuvraxis/taskQ"
	"github.com/Nuvraxis/taskQ/saga"
	"github.com/google/uuid"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
	"github.com/sajanv88/bankstmt-analyzer/internal/ocr"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

// Step names. They appear in logs and, for a failure, in the upload's
// failure_reason, so they are part of the API surface in practice.
const (
	StepMarkProcessing = "mark_processing"
	StepOCR            = "ocr"
	StepAnalyze        = "analyze"
	StepMarkCompleted  = "mark_completed"
)

// terminalWriteTimeout bounds the write that records a run's final status.
const terminalWriteTimeout = 10 * time.Second

// State is the saga's payload. It is JSON-marshalled onto the queue on
// every hop, so it holds identifiers only: the OCR markdown and the
// analysis itself live in Postgres, where the next hop reads them.
type State struct {
	UploadID uuid.UUID `json:"upload_id"`
	// AnalysisID is set by the analyze step and used for logging. The
	// compensation deletes by upload id, so nothing depends on it.
	AnalysisID uuid.UUID `json:"analysis_id,omitempty"`
}

// Store is the persistence the pipeline needs. Declared here so the tests
// can drive the steps against a fake; *db.Store satisfies it.
type Store interface {
	SetUploadStatus(ctx context.Context, arg db.SetUploadStatusParams) (db.Upload, error)
	ListUploadFiles(ctx context.Context, uploadID uuid.UUID) ([]db.UploadFile, error)
	SetUploadFileOCR(ctx context.Context, arg db.SetUploadFileOCRParams) error
	ReplaceAnalysis(ctx context.Context, in db.NewAnalysis) (db.Analysis, error)
	DeleteAnalysisByUpload(ctx context.Context, uploadID uuid.UUID) (int64, error)
}

// OCRClient extracts markdown from a statement PDF.
type OCRClient interface {
	Extract(ctx context.Context, pdf []byte) (ocr.Result, error)
}

// LLMClient turns statement markdown into the structured analysis.
type LLMClient interface {
	Analyze(ctx context.Context, statements []llm.Statement) (*llm.Response, error)
}

// Deps are the pipeline's collaborators.
type Deps struct {
	Broker taskq.Broker
	Store  Store
	Blobs  storage.BlobStore
	OCR    OCRClient
	LLM    LLMClient
	Logger *slog.Logger
	Queue  config.QueueConfig
	Worker config.WorkerConfig
	// Secrets are scrubbed from any error before it is logged, carried
	// through the queue, or written to failure_reason.
	Secrets []string
}

func (d Deps) validate() error {
	switch {
	case d.Broker == nil:
		return errors.New("pipeline: Deps.Broker is required")
	case d.Store == nil:
		return errors.New("pipeline: Deps.Store is required")
	case d.Blobs == nil:
		return errors.New("pipeline: Deps.Blobs is required")
	case d.OCR == nil:
		return errors.New("pipeline: Deps.OCR is required")
	case d.LLM == nil:
		return errors.New("pipeline: Deps.LLM is required")
	case d.Logger == nil:
		return errors.New("pipeline: Deps.Logger is required")
	case d.Queue.Name == "":
		return errors.New("pipeline: Deps.Queue.Name is required")
	default:
		return nil
	}
}

// Pipeline enqueues and runs analysis sagas.
type Pipeline struct {
	deps      Deps
	saga      *saga.Saga[State]
	scrubber  scrubber
	logger    *slog.Logger
	queueName string
}

// New builds the saga and its step handlers.
func New(deps Deps) (*Pipeline, error) {
	if err := deps.validate(); err != nil {
		return nil, err
	}

	p := &Pipeline{
		deps:      deps,
		scrubber:  newScrubber(deps.Secrets),
		logger:    deps.Logger.With("component", "pipeline"),
		queueName: deps.Queue.Name,
	}

	p.saga = saga.New(deps.Broker, deps.Queue.Name, p.steps(),
		saga.WithMaxRetry[State](deps.Worker.MaxRetry),
		saga.WithOnComplete(p.onComplete),
		saga.WithOnFailed(p.onFailed),
		saga.WithOnCompensationFailed(p.onCompensationFailed),
	)
	return p, nil
}

// steps is the workflow, in order.
//
// Only analyze has a compensation. mark_processing has nothing to undo —
// the failed status is written by the onFailed hook, which also covers a
// failure at step 0 where no compensation runs at all. The ocr step's
// output is deliberately kept on rollback: it is per-file cached markdown,
// not part of the analysis, and keeping it makes a re-run cheaper without
// making it wrong.
func (p *Pipeline) steps() []saga.Step[State] {
	return []saga.Step[State]{
		{Name: StepMarkProcessing, Do: p.instrument(StepMarkProcessing, p.markProcessing)},
		{Name: StepOCR, Do: p.instrument(StepOCR, p.runOCR)},
		{
			Name:       StepAnalyze,
			Do:         p.instrument(StepAnalyze, p.analyze),
			Compensate: p.instrument(StepAnalyze+"_compensate", p.deleteAnalysis),
		},
		{Name: StepMarkCompleted, Do: p.instrument(StepMarkCompleted, p.markCompleted)},
	}
}

// Enqueue starts a saga run for an upload. It satisfies the API's Enqueuer.
func (p *Pipeline) Enqueue(ctx context.Context, uploadID uuid.UUID) error {
	sagaID, err := p.saga.Start(ctx, State{UploadID: uploadID})
	if err != nil {
		return fmt.Errorf("pipeline: start saga for upload %s: %w", uploadID, err)
	}
	p.logger.InfoContext(ctx, "analysis queued", "upload_id", uploadID.String(), "saga_id", sagaID)
	return nil
}

// Run consumes the queue until ctx is cancelled, then waits for in-flight
// steps to finish.
func (p *Pipeline) Run(ctx context.Context) error {
	pool := taskq.NewPool(p.deps.Broker, p.queueName, p.saga.Handler(),
		taskq.WithConcurrency(p.deps.Worker.Concurrency),
		taskq.WithBackoffStrategy(taskq.ExponentialBackoff{
			Base:   p.deps.Worker.BackoffBase,
			Max:    p.deps.Worker.BackoffMax,
			Factor: 2,
			Jitter: true,
		}),
	)

	p.logger.InfoContext(ctx, "worker started",
		"queue", p.queueName,
		"concurrency", p.deps.Worker.Concurrency,
		"max_retry", p.deps.Worker.MaxRetry,
	)
	if err := pool.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pipeline: worker pool: %w", err)
	}
	p.logger.Info("worker stopped", "queue", p.queueName)
	return nil
}

// instrument wraps a step with its log line and scrubs its error.
//
// Scrubbing here rather than only in the hooks matters: the saga puts the
// failing error's message into the envelope that travels through Postgres
// to the compensating hops, so an unscrubbed message would be persisted in
// the queue table long before any hook sees it.
func (p *Pipeline) instrument(name string, fn func(context.Context, *State) error) func(context.Context, *State) error {
	return func(ctx context.Context, state *State) error {
		start := time.Now()
		err := fn(ctx, state)

		attrs := []any{
			"upload_id", state.UploadID.String(),
			"step", name,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if err != nil {
			clean := p.scrubber.message(err)
			p.logger.ErrorContext(ctx, "pipeline step failed",
				append(attrs, "error", clean, "retryable", isRetryable(err))...)
			return errors.New(clean)
		}
		p.logger.InfoContext(ctx, "pipeline step completed", attrs...)
		return nil
	}
}

func (p *Pipeline) onComplete(ctx context.Context, sagaID string, state State) {
	p.logger.InfoContext(ctx, "analysis completed",
		"upload_id", state.UploadID.String(),
		"analysis_id", state.AnalysisID.String(),
		"saga_id", sagaID,
	)
}

// onFailed records the terminal failure. It fires once a run cannot
// complete, after any compensation has unwound, and always reports the
// step whose Do originally failed.
func (p *Pipeline) onFailed(ctx context.Context, sagaID string, state State, failedStep string, cause error) {
	reason := failedStep + ": " + p.scrubber.message(cause)
	p.logger.ErrorContext(ctx, "analysis failed",
		"upload_id", state.UploadID.String(),
		"saga_id", sagaID,
		"step", failedStep,
		"reason", reason,
	)
	p.markFailed(ctx, state, reason)
}

// onCompensationFailed fires when a rollback could not finish. The run is
// left partly compensated and no further automatic recovery is possible,
// so this is logged at error and the upload is still marked failed rather
// than being left in processing forever.
func (p *Pipeline) onCompensationFailed(ctx context.Context, sagaID string, state State, step string, cause error) {
	reason := step + ": rollback failed: " + p.scrubber.message(cause)
	p.logger.ErrorContext(ctx, "analysis compensation failed, manual cleanup may be required",
		"upload_id", state.UploadID.String(),
		"saga_id", sagaID,
		"step", step,
		"reason", reason,
	)
	p.markFailed(ctx, state, reason)
}

// markFailed writes the terminal status on a context that outlives the
// hop's own. The hook can fire while the worker is shutting down, and an
// upload left claiming to be processing with nothing queued for it is
// worse than a write that runs a moment past cancellation.
func (p *Pipeline) markFailed(ctx context.Context, state State, reason string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalWriteTimeout)
	defer cancel()

	reason = truncate(reason, maxFailureReasonBytes)
	if _, err := p.deps.Store.SetUploadStatus(writeCtx, db.SetUploadStatusParams{
		ID:            state.UploadID,
		Status:        db.StatusFailed,
		FailureReason: &reason,
	}); err != nil {
		p.logger.ErrorContext(writeCtx, "failed to record upload failure",
			"upload_id", state.UploadID.String(), "error", p.scrubber.message(err))
	}
}

// readBlob loads one stored PDF in full. OCR needs the whole document, so
// there is nothing to stream.
func (p *Pipeline) readBlob(ctx context.Context, key string) ([]byte, error) {
	reader, err := p.deps.Blobs.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("open stored file: %w", err)
	}
	defer func() { _ = reader.Close() }()

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read stored file: %w", err)
	}
	return body, nil
}

// retryable is implemented by the upstream clients' error types.
type retryable interface{ Retryable() bool }

// isRetryable reports whether an error describes a condition another
// attempt could clear. It is recorded on the failure log line; taskQ
// retries every failed hop regardless, up to its own budget.
func isRetryable(err error) bool {
	var r retryable
	if errors.As(err, &r) {
		return r.Retryable()
	}
	return true
}
