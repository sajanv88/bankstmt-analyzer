package pipeline

import (
	"context"
	"fmt"
	"strings"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
)

// markProcessing moves the upload out of pending.
//
// The failure reason is cleared at the same time: a re-run of a previously
// failed upload should not keep advertising the failure it is retrying.
func (p *Pipeline) markProcessing(ctx context.Context, state *State) error {
	if _, err := p.deps.Store.SetUploadStatus(ctx, db.SetUploadStatusParams{
		ID:            state.UploadID,
		Status:        db.StatusProcessing,
		FailureReason: nil,
	}); err != nil {
		return fmt.Errorf("set status to processing: %w", err)
	}
	return nil
}

// runOCR extracts markdown for every file of the upload.
//
// Files that already carry OCR output are skipped, which is what makes the
// step idempotent: taskQ delivers at least once, so a worker that crashed
// after extracting three of five files resumes with the remaining two
// rather than paying for all five again.
func (p *Pipeline) runOCR(ctx context.Context, state *State) error {
	files, err := p.deps.Store.ListUploadFiles(ctx, state.UploadID)
	if err != nil {
		return fmt.Errorf("list upload files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("upload has no files to process")
	}

	for _, file := range files {
		if hasOCR(file) {
			p.logger.DebugContext(ctx, "skipping file that already has OCR output",
				"upload_id", state.UploadID.String(), "filename", file.Filename)
			continue
		}
		if err := p.extractOne(ctx, file); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pipeline) extractOne(ctx context.Context, file db.UploadFile) error {
	pdf, err := p.readBlob(ctx, file.StorageKey)
	if err != nil {
		return fmt.Errorf("file %q: %w", file.Filename, err)
	}

	result, err := p.deps.OCR.Extract(ctx, pdf)
	if err != nil {
		return fmt.Errorf("file %q: %w", file.Filename, err)
	}

	markdown := result.Markdown()
	if strings.TrimSpace(markdown) == "" {
		return fmt.Errorf("file %q: the document produced no text", file.Filename)
	}
	pages := int32(result.PageCount())

	if err := p.deps.Store.SetUploadFileOCR(ctx, db.SetUploadFileOCRParams{
		ID:          file.ID,
		OcrMarkdown: &markdown,
		PageCount:   &pages,
	}); err != nil {
		return fmt.Errorf("store OCR output for %q: %w", file.Filename, err)
	}
	return nil
}

// analyze hands every statement to the model and stores what comes back.
func (p *Pipeline) analyze(ctx context.Context, state *State) error {
	files, err := p.deps.Store.ListUploadFiles(ctx, state.UploadID)
	if err != nil {
		return fmt.Errorf("list upload files: %w", err)
	}

	statements := make([]llm.Statement, 0, len(files))
	for _, file := range files {
		if !hasOCR(file) {
			// The ocr step is supposed to guarantee this. Failing here
			// rather than silently analysing a subset keeps a partial
			// analysis from being presented as a complete one.
			return fmt.Errorf("file %q has no OCR output", file.Filename)
		}
		statements = append(statements, llm.Statement{
			Filename: file.Filename,
			Markdown: *file.OcrMarkdown,
		})
	}
	if len(statements) == 0 {
		return fmt.Errorf("upload has no statements to analyse")
	}

	response, err := p.deps.LLM.Analyze(ctx, statements)
	if err != nil {
		return fmt.Errorf("analyse statements: %w", err)
	}

	params, err := buildAnalysis(state.UploadID, response)
	if err != nil {
		return err
	}

	analysis, err := p.deps.Store.ReplaceAnalysis(ctx, params)
	if err != nil {
		return fmt.Errorf("store analysis: %w", err)
	}
	state.AnalysisID = analysis.ID

	p.logger.InfoContext(ctx, "analysis stored",
		"upload_id", state.UploadID.String(),
		"analysis_id", analysis.ID.String(),
		"statements", len(statements),
		"transactions", len(params.Transactions),
	)
	return nil
}

// deleteAnalysis is the analyze step's compensation: it removes the
// analysis, and with it the transactions that cascade from it, so a failed
// run leaves nothing half-written for the visualization endpoint to serve.
func (p *Pipeline) deleteAnalysis(ctx context.Context, state *State) error {
	deleted, err := p.deps.Store.DeleteAnalysisByUpload(ctx, state.UploadID)
	if err != nil {
		return fmt.Errorf("delete partial analysis: %w", err)
	}
	p.logger.InfoContext(ctx, "rolled back stored analysis",
		"upload_id", state.UploadID.String(), "analyses_deleted", deleted)
	return nil
}

// markCompleted is the final step; reaching it means every earlier step
// succeeded.
func (p *Pipeline) markCompleted(ctx context.Context, state *State) error {
	if _, err := p.deps.Store.SetUploadStatus(ctx, db.SetUploadStatusParams{
		ID:            state.UploadID,
		Status:        db.StatusCompleted,
		FailureReason: nil,
	}); err != nil {
		return fmt.Errorf("set status to completed: %w", err)
	}
	return nil
}

func hasOCR(file db.UploadFile) bool {
	return file.OcrMarkdown != nil && strings.TrimSpace(*file.OcrMarkdown) != ""
}
