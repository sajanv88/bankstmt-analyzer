package http_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
)

// fakePinger stands in for the pgx pool behind /readyz.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func newTestRouter(t *testing.T, pinger apihttp.Pinger) http.Handler {
	t.Helper()
	handler, err := apihttp.NewRouter(apihttp.Deps{
		Config: config.Config{Env: config.EnvDevelopment},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:     pinger,
	})
	require.NoError(t, err)
	return handler
}

func TestHealthAndReadinessProbes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		target       string
		method       string
		pinger       apihttp.Pinger
		wantStatus   int
		wantJSONType string
	}{
		{
			name:         "liveness is up regardless of the database",
			target:       "/healthz",
			method:       http.MethodGet,
			pinger:       fakePinger{err: errors.New("database is down")},
			wantStatus:   http.StatusOK,
			wantJSONType: "application/json",
		},
		{
			name:         "readiness is up when the database answers",
			target:       "/readyz",
			method:       http.MethodGet,
			pinger:       fakePinger{},
			wantStatus:   http.StatusOK,
			wantJSONType: "application/json",
		},
		{
			name:         "readiness fails when the database does not answer",
			target:       "/readyz",
			method:       http.MethodGet,
			pinger:       fakePinger{err: errors.New("connection refused")},
			wantStatus:   http.StatusServiceUnavailable,
			wantJSONType: apihttp.ProblemContentType,
		},
		{
			name:         "unknown routes return a problem document",
			target:       "/does-not-exist",
			method:       http.MethodGet,
			pinger:       fakePinger{},
			wantStatus:   http.StatusNotFound,
			wantJSONType: apihttp.ProblemContentType,
		},
		{
			name:         "wrong method returns a problem document",
			target:       "/healthz",
			method:       http.MethodDelete,
			pinger:       fakePinger{},
			wantStatus:   http.StatusMethodNotAllowed,
			wantJSONType: apihttp.ProblemContentType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			router := newTestRouter(t, tc.pinger)

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), tc.wantJSONType)

			if tc.wantJSONType != apihttp.ProblemContentType {
				var body apihttp.HealthResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, "ok", body.Status)
				return
			}

			var problem apihttp.Problem
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
			assert.Equal(t, tc.wantStatus, problem.Status)
			assert.NotEmpty(t, problem.Title)
			assert.Equal(t, tc.target, problem.Instance)
			assert.NotEmpty(t, problem.RequestID, "the request id middleware should populate every problem")
		})
	}
}

// TestRecovererReturnsProblem proves a panicking handler still produces a
// well-formed RFC 7807 response rather than a dropped connection.
func TestRecovererReturnsProblem(t *testing.T) {
	t.Parallel()

	router := newTestRouter(t, fakePinger{})
	chiRouter, ok := router.(interface {
		Get(pattern string, h http.HandlerFunc)
		ServeHTTP(http.ResponseWriter, *http.Request)
	})
	require.True(t, ok, "the router should expose chi's Get for this test")
	chiRouter.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("handler exploded") })

	rec := httptest.NewRecorder()
	chiRouter.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), apihttp.ProblemContentType)

	var problem apihttp.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, http.StatusInternalServerError, problem.Status)
	// The panic value must not reach the client.
	assert.NotContains(t, problem.Detail, "exploded")
}

func TestNewRouterRequiresDependencies(t *testing.T) {
	t.Parallel()

	_, err := apihttp.NewRouter(apihttp.Deps{Logger: nil, DB: fakePinger{}})
	require.Error(t, err)

	_, err = apihttp.NewRouter(apihttp.Deps{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	require.Error(t, err)
}
