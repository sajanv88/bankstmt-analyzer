package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Nuvraxis/taskQ/membroker"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
	"github.com/sajanv88/bankstmt-analyzer/internal/ocr"
	"github.com/sajanv88/bankstmt-analyzer/internal/pipeline"
)

// The saga is driven end to end over taskQ's in-memory broker rather than
// by calling the steps directly. That is the only way the parts worth
// testing — the ordering of hops, the pivot into compensation, and which
// terminal status is written — are actually exercised; calling a step
// function proves nothing about the workflow around it.

// testAPIKey is planted in upstream error messages to prove it never
// reaches a log field or the stored failure reason.
const testAPIKey = "sk-secret-key-do-not-leak-0123456789"

type harness struct {
	pipeline *pipeline.Pipeline
	store    *fakeStore
	blobs    *fakeBlobs
	ocr      *fakeOCR
	llm      *fakeLLM
	uploadID uuid.UUID
}

// newHarness wires a pipeline over an in-memory broker, seeds one upload
// with two statement files, and runs the worker until the test ends.
func newHarness(t *testing.T, tweak func(*harness)) *harness {
	t.Helper()

	uploadID := uuid.New()
	h := &harness{
		store:    newFakeStore(),
		blobs:    newFakeBlobs(),
		ocr:      &fakeOCR{result: canonicalOCR()},
		llm:      &fakeLLM{response: canonicalResponse(t)},
		uploadID: uploadID,
	}

	files := []db.UploadFile{
		{ID: uuid.New(), UploadID: uploadID, Position: 0, Filename: "january.pdf", StorageKey: "uploads/a.pdf"},
		{ID: uuid.New(), UploadID: uploadID, Position: 1, Filename: "february.pdf", StorageKey: "uploads/b.pdf"},
	}
	h.store.seedUpload(uploadID, files)
	h.blobs.put("uploads/a.pdf", []byte("%PDF-1.7 january"))
	h.blobs.put("uploads/b.pdf", []byte("%PDF-1.7 february"))

	if tweak != nil {
		tweak(h)
	}

	broker := membroker.New()
	t.Cleanup(broker.Close)

	built, err := pipeline.New(pipeline.Deps{
		Broker: broker,
		Store:  h.store,
		Blobs:  h.blobs,
		OCR:    h.ocr,
		LLM:    h.llm,
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Queue:  config.QueueConfig{Name: "test-analysis"},
		// No retries: every failure in these tests is deterministic, so a
		// retry budget would only make them slower.
		Worker:  config.WorkerConfig{Concurrency: 1, MaxRetry: 0, BackoffBase: time.Millisecond, BackoffMax: time.Millisecond},
		Secrets: []string{testAPIKey},
	})
	require.NoError(t, err)
	h.pipeline = built

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = built.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the worker did not stop after its context was cancelled")
		}
	})

	return h
}

// waitForStatus blocks until the upload reaches want, or fails the test.
func (h *harness) waitForStatus(t *testing.T, want string) db.Upload {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		upload, ok := h.store.upload(h.uploadID)
		if ok {
			last = upload.Status
			if upload.Status == want {
				return upload
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("upload never reached status %q (last seen %q)", want, last)
	return db.Upload{}
}

func canonicalOCR() ocr.Result {
	return ocr.Result{Pages: []ocr.Page{
		{Index: 0, Markdown: "# Statement\n\n| date | amount |"},
		{Index: 1, Markdown: "continued"},
	}}
}

// canonicalResponse is a well-formed model reply with two transactions.
func canonicalResponse(t *testing.T) *llm.Response {
	t.Helper()
	raw := []byte(`{
	  "analysis": {
	    "currency": "EUR",
	    "period_start": "2025-01-01",
	    "period_end": "2025-02-28",
	    "key_insights": ["spending is stable"]
	  },
	  "savings_plan": {"target": 500},
	  "chart_data": {
	    "present": {"monthly": []},
	    "forecast": {"projected_savings": [{"month": "2025-03", "amount": 500}]}
	  },
	  "transactions": [
	    {"date": "2025-01-05", "description": "Salary", "counterparty": "ACME",
	     "amount": 3000.00, "direction": "credit", "balance_after": 3000.00,
	     "category": "Salary", "essential": false, "recurring": true},
	    {"date": "2025-01-09", "description": "Groceries", "counterparty": "Shop",
	     "amount": 42.75, "direction": "debit", "balance_after": 2957.25,
	     "category": "Groceries", "essential": true, "recurring": false}
	  ]
	}`)

	var resp llm.Response
	require.NoError(t, json.Unmarshal(raw, &resp))
	resp.Raw = raw
	return &resp
}

func TestPipelineHappyPath(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)
	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))

	upload := h.waitForStatus(t, db.StatusCompleted)
	assert.Nil(t, upload.FailureReason, "a completed upload carries no failure reason")

	// Every file went through OCR and its markdown was stored.
	assert.Equal(t, 2, h.ocr.callCount())
	assert.Equal(t, 2, h.store.ocrWriteCount())

	// The model saw both statements, in upload order.
	statements := h.llm.lastStatements()
	require.Len(t, statements, 2)
	assert.Equal(t, "january.pdf", statements[0].Filename)
	assert.Equal(t, "february.pdf", statements[1].Filename)
	assert.Contains(t, statements[0].Markdown, "# Statement")

	// The analysis and its transactions were stored.
	stored := h.store.storedAnalyses()
	require.Len(t, stored, 1)
	assert.Equal(t, "EUR", stored[0].Analysis.Currency)
	require.Len(t, stored[0].Transactions, 2)
	assert.Equal(t, db.DirectionCredit, stored[0].Transactions[0].Direction)
	assert.Equal(t, db.DirectionDebit, stored[0].Transactions[1].Direction)
	assert.Equal(t, "Groceries", stored[0].Transactions[1].Category)
	assert.True(t, stored[0].Transactions[1].Essential)

	assert.Zero(t, h.store.deletions(), "a successful run compensates nothing")
}

// TestPipelineUserMessageLayout pins the format the statements are
// presented in, which the prompt depends on.
func TestPipelineUserMessageLayout(t *testing.T) {
	t.Parallel()

	message := llm.BuildUserMessage([]llm.Statement{
		{Markdown: "first"},
		{Markdown: "second"},
	})

	assert.Equal(t, "--- STATEMENT 1 ---\nfirst\n\n--- STATEMENT 2 ---\nsecond", message)
}

func TestPipelineOCRFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(h *harness) {
		h.ocr.err = &ocr.APIError{
			StatusCode: 500,
			Status:     "500 Internal Server Error",
			Body:       "upstream exploded while using key " + testAPIKey,
		}
	})
	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))

	upload := h.waitForStatus(t, db.StatusFailed)
	require.NotNil(t, upload.FailureReason)
	reason := *upload.FailureReason

	assert.True(t, strings.HasPrefix(reason, pipeline.StepOCR+": "),
		"the reason should name the step that failed, got %q", reason)
	assert.NotContains(t, reason, testAPIKey, "credentials must never be persisted")
	assert.Contains(t, reason, "january.pdf", "the reason should say which file failed")

	// Nothing downstream ran, so there is nothing to compensate.
	assert.Zero(t, h.llm.callCount(), "the model should not be called when OCR failed")
	assert.Empty(t, h.store.storedAnalyses())
	_, hasAnalysis := h.store.analysisFor(h.uploadID)
	assert.False(t, hasAnalysis)
}

func TestPipelineInvalidLLMJSON(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(h *harness) {
		h.llm.err = &llm.InvalidResponseError{
			Reason: "the reply is missing the required top-level keys [chart_data transactions]",
		}
	})
	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))

	upload := h.waitForStatus(t, db.StatusFailed)
	require.NotNil(t, upload.FailureReason)
	reason := *upload.FailureReason

	assert.True(t, strings.HasPrefix(reason, pipeline.StepAnalyze+": "),
		"the reason should name the analyze step, got %q", reason)
	assert.Contains(t, reason, "missing the required top-level keys")

	assert.Empty(t, h.store.storedAnalyses(), "an invalid reply stores no analysis")
	// OCR still ran and its output is deliberately kept, so a re-run is
	// cheap.
	assert.Equal(t, 2, h.ocr.callCount())
}

// TestPipelineCompensatesStoredAnalysis is the case compensation exists
// for: analyze succeeded and wrote rows, then a later step failed, so the
// rows must not survive.
func TestPipelineCompensatesStoredAnalysis(t *testing.T) {
	t.Parallel()

	h := newHarness(t, func(h *harness) {
		h.store.setStatusErr[db.StatusCompleted] = errors.New("database went away")
	})
	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))

	upload := h.waitForStatus(t, db.StatusFailed)
	require.NotNil(t, upload.FailureReason)
	assert.True(t, strings.HasPrefix(*upload.FailureReason, pipeline.StepMarkCompleted+": "),
		"got %q", *upload.FailureReason)

	assert.Positive(t, h.store.deletions(), "the analyze step should have been compensated")
	_, hasAnalysis := h.store.analysisFor(h.uploadID)
	assert.False(t, hasAnalysis, "the partial analysis must be rolled back")
}

// TestPipelineIsIdempotentForTheSameUpload proves a re-run does not redo
// the OCR it already paid for, nor duplicate the analysis.
func TestPipelineIsIdempotentForTheSameUpload(t *testing.T) {
	t.Parallel()

	h := newHarness(t, nil)

	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))
	h.waitForStatus(t, db.StatusCompleted)
	require.Equal(t, 2, h.ocr.callCount())

	// Re-run the same upload, exactly as a redelivery or a manual retry
	// would.
	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && h.llm.callCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}

	assert.Equal(t, 2, h.ocr.callCount(),
		"files that already have OCR output should not be extracted again")
	assert.Equal(t, 2, h.llm.callCount(), "the analysis itself is redone")

	upload := h.waitForStatus(t, db.StatusCompleted)
	assert.Nil(t, upload.FailureReason)
}

// TestPipelineClearsPreviousFailureOnRerun proves a retried upload does not
// keep advertising the failure it is retrying.
func TestPipelineClearsPreviousFailureOnRerun(t *testing.T) {
	t.Parallel()

	stale := "ocr: something went wrong earlier"
	h := newHarness(t, func(h *harness) {
		upload, _ := h.store.upload(h.uploadID)
		upload.Status = db.StatusFailed
		upload.FailureReason = &stale
		h.store.uploads[h.uploadID] = upload
	})

	require.NoError(t, h.pipeline.Enqueue(t.Context(), h.uploadID))
	upload := h.waitForStatus(t, db.StatusCompleted)

	assert.Nil(t, upload.FailureReason, "the stale reason should have been cleared")
}

func TestNewRequiresDependencies(t *testing.T) {
	t.Parallel()

	broker := membroker.New()
	t.Cleanup(broker.Close)

	full := func() pipeline.Deps {
		return pipeline.Deps{
			Broker: broker,
			Store:  newFakeStore(),
			Blobs:  newFakeBlobs(),
			OCR:    &fakeOCR{},
			LLM:    &fakeLLM{},
			Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			Queue:  config.QueueConfig{Name: "q"},
		}
	}

	tests := []struct {
		name    string
		omit    func(*pipeline.Deps)
		wantMsg string
	}{
		{name: "broker", omit: func(d *pipeline.Deps) { d.Broker = nil }, wantMsg: "Deps.Broker"},
		{name: "store", omit: func(d *pipeline.Deps) { d.Store = nil }, wantMsg: "Deps.Store"},
		{name: "blobs", omit: func(d *pipeline.Deps) { d.Blobs = nil }, wantMsg: "Deps.Blobs"},
		{name: "ocr", omit: func(d *pipeline.Deps) { d.OCR = nil }, wantMsg: "Deps.OCR"},
		{name: "llm", omit: func(d *pipeline.Deps) { d.LLM = nil }, wantMsg: "Deps.LLM"},
		{name: "logger", omit: func(d *pipeline.Deps) { d.Logger = nil }, wantMsg: "Deps.Logger"},
		{name: "queue name", omit: func(d *pipeline.Deps) { d.Queue.Name = "" }, wantMsg: "Deps.Queue.Name"},
	}
	for _, tc := range tests {
		t.Run("missing "+tc.name, func(t *testing.T) {
			t.Parallel()
			deps := full()
			tc.omit(&deps)

			_, err := pipeline.New(deps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}
