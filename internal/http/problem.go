// Package http contains the chi router, its middleware, the request and
// response DTOs, and the HTTP handlers.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5/middleware"
)

// ProblemContentType is the media type mandated by RFC 7807.
const ProblemContentType = "application/problem+json"

// Problem is an RFC 7807 problem details object. Every error this service
// returns is serialised as one.
type Problem struct {
	// Type is a URI reference identifying the problem kind. "about:blank"
	// means the status code alone describes it, which RFC 7807 defines as
	// the default.
	Type string `json:"type"`
	// Title is a short, human-readable summary, stable for a given Type.
	Title string `json:"title"`
	// Status repeats the HTTP status code, so the body stands alone.
	Status int `json:"status"`
	// Detail explains this specific occurrence. It never contains
	// credentials or internal stack traces.
	Detail string `json:"detail,omitempty"`
	// Instance identifies this occurrence — here, the request path.
	Instance string `json:"instance,omitempty"`
	// RequestID is an extension member that ties a response to its log
	// lines.
	RequestID string `json:"request_id,omitempty"`
	// InvalidParams is an extension member listing per-field validation
	// failures.
	InvalidParams []InvalidParam `json:"invalid_params,omitempty"`
}

// InvalidParam describes one field-level validation failure.
type InvalidParam struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Error makes Problem usable as an error, so handlers can build one and
// return it through the ordinary error path.
func (p *Problem) Error() string { return p.Title + ": " + p.Detail }

// NewProblem builds a Problem with "about:blank" as its type, which is the
// right choice unless the service publishes a documentation URI for the
// specific failure.
func NewProblem(status int, detail string) *Problem {
	return &Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	}
}

// WriteProblem serialises p as application/problem+json, filling in the
// instance and request id from r. A nil or zero-status problem is treated
// as a 500 so a programming slip still produces a valid response.
func WriteProblem(w http.ResponseWriter, r *http.Request, logger *slog.Logger, p *Problem) {
	if p == nil {
		p = NewProblem(http.StatusInternalServerError, "")
	}
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	if p.RequestID == "" && r != nil {
		p.RequestID = middleware.GetReqID(r.Context())
	}

	w.Header().Set("Content-Type", ProblemContentType)
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil && logger != nil {
		// The status line is already on the wire, so there is nothing to
		// do but record it.
		logger.ErrorContext(r.Context(), "failed to encode problem response", "error", err)
	}
}

// WriteError maps err onto a problem response: a *Problem is written as
// authored, anything else becomes an opaque 500 whose detail stays generic
// so internal messages never reach the client.
func WriteError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var p *Problem
	if errors.As(err, &p) {
		WriteProblem(w, r, logger, p)
		return
	}
	if logger != nil {
		logger.ErrorContext(r.Context(), "unhandled request error", "error", err)
	}
	WriteProblem(w, r, logger, NewProblem(http.StatusInternalServerError, "An unexpected error occurred."))
}

// WriteJSON serialises v as application/json with the given status.
func WriteJSON(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil && logger != nil {
		logger.ErrorContext(r.Context(), "failed to encode response body", "error", err)
	}
}
