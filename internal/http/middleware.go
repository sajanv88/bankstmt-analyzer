package http

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

// APIKeyHeader is the header a caller presents its key in. It is exported
// so the CORS allow-list and the tests name the same string the middleware
// reads.
const APIKeyHeader = "api-key"

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

// requireAPIKey rejects any request that does not carry the configured key
// in the api-key header.
//
// The comparison is constant time. A byte-by-byte one returns sooner the
// earlier it finds a difference, which over enough requests lets a caller
// recover the key one character at a time. subtle.ConstantTimeCompare also
// reports no match for unequal lengths, so an absent or truncated header
// takes the same path as a wrong one.
//
// The rejection says only that a valid key is required. Distinguishing
// "missing" from "malformed" from "wrong" tells an unauthenticated caller
// how close they are, which is precisely what they should not learn. The
// request logger records the 401 at warn level, so the operator still sees
// it.
func requireAPIKey(key string, logger *slog.Logger) func(http.Handler) http.Handler {
	want := []byte(key)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get(APIKeyHeader))
			if subtle.ConstantTimeCompare(got, want) != 1 {
				WriteProblem(w, r, logger, NewProblem(http.StatusUnauthorized,
					"A valid api-key header is required."))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
