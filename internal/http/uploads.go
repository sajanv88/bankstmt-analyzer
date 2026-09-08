package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/storage"
)

const (
	// uploadFieldName is the multipart field the PDFs arrive in.
	uploadFieldName = "files"
	// pdfMagic opens every PDF file. The client-supplied Content-Type is
	// not evidence of anything, so this is what decides.
	pdfMagic = "%PDF-"
	// multipartOverhead is the slack added to the request body limit to
	// cover part headers and boundaries, so a request carrying the maximum
	// permitted file bytes is not rejected over its envelope.
	multipartOverhead = 1 << 20
)

type uploadHandler struct {
	store    UploadStore
	blobs    storage.BlobStore
	enqueuer Enqueuer
	cfg      config.UploadConfig
	logger   *slog.Logger
}

func newUploadHandler(
	store UploadStore,
	blobs storage.BlobStore,
	enqueuer Enqueuer,
	cfg config.UploadConfig,
	logger *slog.Logger,
) *uploadHandler {
	return &uploadHandler{store: store, blobs: blobs, enqueuer: enqueuer, cfg: cfg, logger: logger}
}

// create accepts the statement PDFs and starts the analysis.
//
//	@Summary		Upload bank statements
//	@Description	Accepts between 1 and 12 PDF bank statements as multipart/form-data under the field `files`, each at most 20 MB. Files are validated by magic bytes rather than by the client-supplied content type. The response returns immediately; poll the status endpoint for progress.
//	@Tags			uploads
//	@Accept			multipart/form-data
//	@Produce		json
//	@Param			files	formData	file	true	"Bank statement PDFs (repeat the field once per file)"
//	@Success		202		{object}	CreateUploadResponse
//	@Failure		400		{object}	Problem	"Malformed request, no files, too many files, or a file that is not a PDF"
//	@Failure		401		{object}	Problem	"Missing or invalid api-key header"
//	@Failure		413		{object}	Problem	"A file, or the request as a whole, is too large"
//	@Failure		500		{object}	Problem
//	@Failure		503		{object}	Problem	"The upload was stored but could not be queued for analysis"
//	@Security		ApiKeyAuth
//	@Router			/api/v1/uploads [post]
func (h *uploadHandler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Bound the request before reading any of it, so an oversized body is
	// refused rather than buffered.
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxFileBytes*int64(h.cfg.MaxFiles)+multipartOverhead)

	reader, err := r.MultipartReader()
	if err != nil {
		WriteProblem(w, r, h.logger, NewProblem(http.StatusBadRequest,
			"The request body must be multipart/form-data."))
		return
	}

	uploadID := uuid.New()
	files, problem := h.readFiles(ctx, reader, uploadID)
	if problem != nil {
		h.discard(ctx, files)
		WriteProblem(w, r, h.logger, problem)
		return
	}

	upload, err := h.store.CreateUploadWithFiles(ctx, uploadID, files)
	if err != nil {
		// Without an upload row nothing will ever reference these blobs,
		// so they go now rather than becoming orphans.
		h.discard(ctx, files)
		WriteError(w, r, h.logger, err)
		return
	}

	if err := h.enqueuer.Enqueue(ctx, uploadID); err != nil {
		// The rows are committed. taskQ enqueues over its own connection
		// and cannot join that transaction, so this is the one window
		// where an upload can exist with no queued work: close it by
		// recording the failure rather than leaving it pending forever.
		h.markUnqueued(ctx, uploadID, err)
		WriteProblem(w, r, h.logger, NewProblem(http.StatusServiceUnavailable,
			"The upload was stored but could not be queued for analysis. Please retry."))
		return
	}

	h.logger.InfoContext(ctx, "upload accepted",
		"upload_id", uploadID.String(),
		"file_count", len(files),
	)
	WriteJSON(w, r, h.logger, http.StatusAccepted, CreateUploadResponse{
		ID:     upload.ID.String(),
		Status: upload.Status,
	})
}

// readFiles streams every `files` part into the blob store. On failure it
// returns whatever it had already stored alongside the problem, so the
// caller can clean those blobs up.
func (h *uploadHandler) readFiles(
	ctx context.Context,
	reader *multipart.Reader,
	uploadID uuid.UUID,
) ([]db.NewUploadFile, *Problem) {
	var files []db.NewUploadFile

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, h.problemForReadError(err, "The upload could not be read.")
		}

		if part.FormName() != uploadFieldName {
			_ = part.Close()
			continue
		}
		if len(files) >= h.cfg.MaxFiles {
			_ = part.Close()
			return files, NewProblem(http.StatusBadRequest,
				fmt.Sprintf("At most %d files may be uploaded at once.", h.cfg.MaxFiles))
		}

		file, problem := h.readOneFile(ctx, part, uploadID, int32(len(files)))
		_ = part.Close()
		if problem != nil {
			return files, problem
		}
		files = append(files, file)
	}

	if len(files) == 0 {
		return nil, NewProblem(http.StatusBadRequest,
			fmt.Sprintf("At least one PDF must be supplied in the %q field.", uploadFieldName))
	}
	return files, nil
}

// readOneFile validates one part and streams it to the blob store.
func (h *uploadHandler) readOneFile(
	ctx context.Context,
	part *multipart.Part,
	uploadID uuid.UUID,
	position int32,
) (db.NewUploadFile, *Problem) {
	filename := sanitiseFilename(part.FileName())
	if filename == "" {
		return db.NewUploadFile{}, invalidFileProblem("", "the part carries no filename")
	}

	// Check the magic bytes first so a non-PDF never reaches storage.
	head := make([]byte, len(pdfMagic))
	n, err := io.ReadFull(part, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return db.NewUploadFile{}, h.problemForReadError(err, fmt.Sprintf("%q could not be read.", filename))
	}
	if !bytes.HasPrefix(head[:n], []byte(pdfMagic)) {
		return db.NewUploadFile{}, invalidFileProblem(filename,
			"the file does not begin with the PDF signature "+pdfMagic)
	}

	fileID := uuid.New()
	key := storageKey(uploadID, fileID)
	// The magic bytes are already consumed, so they go back in front of
	// the remaining stream. LimitReader takes one byte more than the cap
	// so an oversized file is detected rather than silently truncated.
	body := io.LimitReader(io.MultiReader(bytes.NewReader(head[:n]), part), h.cfg.MaxFileBytes+1)

	size, err := h.blobs.Put(ctx, key, body)
	if err != nil {
		return db.NewUploadFile{}, h.problemForReadError(err, fmt.Sprintf("%q could not be stored.", filename))
	}
	if size > h.cfg.MaxFileBytes {
		h.deleteBlob(ctx, key)
		return db.NewUploadFile{}, NewProblem(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("%q exceeds the %d byte limit for a single file.", filename, h.cfg.MaxFileBytes))
	}

	return db.NewUploadFile{
		ID:          fileID,
		Position:    position,
		Filename:    filename,
		ContentType: contentType(part),
		SizeBytes:   size,
		StorageKey:  key,
	}, nil
}

// problemForReadError separates the client outgrowing the body limit from
// anything else that went wrong mid-stream.
func (h *uploadHandler) problemForReadError(err error, detail string) *Problem {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return NewProblem(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("The request body exceeds the %d byte limit.", maxBytes.Limit))
	}
	return NewProblem(http.StatusBadRequest, detail)
}

// discard removes the blobs written for an upload that will not be recorded.
func (h *uploadHandler) discard(ctx context.Context, files []db.NewUploadFile) {
	for _, f := range files {
		h.deleteBlob(ctx, f.StorageKey)
	}
}

func (h *uploadHandler) deleteBlob(ctx context.Context, key string) {
	if err := h.blobs.Delete(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
		// There is nothing a client can do about a leaked blob, so this is
		// recorded rather than surfaced.
		h.logger.WarnContext(ctx, "failed to remove orphaned upload file",
			"storage_key", key, "error", err)
	}
}

// markUnqueued records that an upload will never be processed, so the
// status endpoint does not report it as pending indefinitely.
func (h *uploadHandler) markUnqueued(ctx context.Context, uploadID uuid.UUID, cause error) {
	h.logger.ErrorContext(ctx, "failed to enqueue upload for analysis",
		"upload_id", uploadID.String(), "error", cause)

	// A fixed reason: the underlying error may name the queue backend or
	// its connection string, and this string is served to clients.
	reason := "enqueue: the upload could not be queued for analysis"
	if _, err := h.store.SetUploadStatus(ctx, db.SetUploadStatusParams{
		ID:            uploadID,
		Status:        db.StatusFailed,
		FailureReason: &reason,
	}); err != nil {
		h.logger.ErrorContext(ctx, "failed to mark unqueued upload as failed",
			"upload_id", uploadID.String(), "error", err)
	}
}

// status reports where an upload has got to.
//
//	@Summary		Get upload status
//	@Description	Reports the processing state of an upload. `failure_reason` is present only when the status is `failed`, and `analysis_id` only when it is `completed`.
//	@Tags			uploads
//	@Produce		json
//	@Param			id	path		string	true	"Upload id"	format(uuid)
//	@Success		200	{object}	UploadStatusResponse
//	@Failure		400	{object}	Problem	"The id is not a uuid"
//	@Failure		401	{object}	Problem	"Missing or invalid api-key header"
//	@Failure		404	{object}	Problem	"No such upload"
//	@Failure		500	{object}	Problem
//	@Security		ApiKeyAuth
//	@Router			/api/v1/uploads/{id}/status [get]
func (h *uploadHandler) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, problem := uploadIDFromRequest(r)
	if problem != nil {
		WriteProblem(w, r, h.logger, problem)
		return
	}

	upload, err := h.store.GetUpload(ctx, id)
	if db.IsNotFound(err) {
		WriteProblem(w, r, h.logger, notFoundProblem(id))
		return
	}
	if err != nil {
		WriteError(w, r, h.logger, fmt.Errorf("load upload %s: %w", id, err))
		return
	}

	resp := UploadStatusResponse{
		ID:        upload.ID.String(),
		Status:    upload.Status,
		CreatedAt: upload.CreatedAt.Time,
		UpdatedAt: upload.UpdatedAt.Time,
	}
	if upload.Status == db.StatusFailed && upload.FailureReason != nil {
		resp.FailureReason = *upload.FailureReason
	}
	if upload.Status == db.StatusCompleted {
		analysisID, err := h.store.GetAnalysisIDByUpload(ctx, id)
		switch {
		case err == nil:
			resp.AnalysisID = analysisID.String()
		case db.IsNotFound(err):
			// Completed with no analysis row should not happen. Report the
			// status honestly rather than failing the request over it.
			h.logger.WarnContext(ctx, "completed upload has no analysis", "upload_id", id.String())
		default:
			WriteError(w, r, h.logger, fmt.Errorf("load analysis for upload %s: %w", id, err))
			return
		}
	}

	WriteJSON(w, r, h.logger, http.StatusOK, resp)
}

// uploadIDFromRequest parses the {id} path parameter.
func uploadIDFromRequest(r *http.Request) (uuid.UUID, *Problem) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		p := NewProblem(http.StatusBadRequest, "The upload id must be a uuid.")
		p.InvalidParams = []InvalidParam{{Name: "id", Reason: "not a valid uuid"}}
		return uuid.Nil, p
	}
	return id, nil
}

func notFoundProblem(id uuid.UUID) *Problem {
	return NewProblem(http.StatusNotFound, fmt.Sprintf("No upload with id %s exists.", id))
}

func invalidFileProblem(filename, reason string) *Problem {
	p := NewProblem(http.StatusBadRequest, "Every uploaded file must be a PDF.")
	name := uploadFieldName
	if filename != "" {
		name = uploadFieldName + "[" + filename + "]"
	}
	p.InvalidParams = []InvalidParam{{Name: name, Reason: reason}}
	return p
}

// storageKey is where an upload's file lives in the BlobStore. Grouping by
// upload id keeps everything for one upload deletable as a unit.
func storageKey(uploadID, fileID uuid.UUID) string {
	return path.Join("uploads", uploadID.String(), fileID.String()+".pdf")
}

// sanitiseFilename reduces a client-supplied name to its final element, so
// a crafted name cannot influence anything that later joins it to a path.
// It is stored for display only; the blob key is built from uuids instead.
func sanitiseFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "." || name == "/" {
		return ""
	}
	return name
}

func contentType(part *multipart.Part) string {
	if ct := part.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "application/pdf"
}
