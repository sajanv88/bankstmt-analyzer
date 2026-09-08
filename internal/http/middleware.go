package http

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// requestLogger emits one structured line per request once it completes.
//
// It logs at Error for 5xx, Warn for 4xx and Info otherwise, so a log level
// of warn in production still surfaces every client and server error.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			// The closure reads the request context directly rather than
			// taking one as a parameter, which is what contextcheck wants
			// but cannot express for a deferred logging hook.
			//nolint:contextcheck // r.Context() is used inside.
			defer func() {
				attrs := []any{
					"request_id", middleware.GetReqID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"status", ww.Status(),
					"bytes", ww.BytesWritten(),
					"duration_ms", time.Since(start).Milliseconds(),
					"remote_addr", r.RemoteAddr,
				}
				logger.Log(r.Context(), levelForStatus(ww.Status()), "http request", attrs...)
			}()

			next.ServeHTTP(ww, r)
		})
	}
}

func levelForStatus(status int) slog.Level {
	switch {
	case status >= http.StatusInternalServerError:
		return slog.LevelError
	case status >= http.StatusBadRequest:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// recoverer turns a panic in a handler into a logged 500 problem response
// instead of a dropped connection.
//
// chi ships its own Recoverer, but it writes a bare status line; this one
// keeps the RFC 7807 contract intact for every error the API returns.
func recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			//nolint:contextcheck // r.Context() is used inside.
			defer func() {
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler is the documented way for a handler
				// to abandon a response; re-panicking preserves that.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}
				logger.ErrorContext(r.Context(), "panic recovered in handler",
					"request_id", middleware.GetReqID(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"panic", rec,
				)
				WriteProblem(w, r, logger, NewProblem(http.StatusInternalServerError, "An unexpected error occurred."))
			}()
			next.ServeHTTP(w, r)
		})
	}
}
