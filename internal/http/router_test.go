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

// testAPIKey is a syntactically valid key: 32 hexadecimal characters.
const testAPIKey = "0123456789abcdef0123456789abcdef"

// fakePinger stands in for the pgx pool behind /readyz.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

// testServer is a router wired to fakes, with the fakes kept to hand so a
// test can seed them beforehand and inspect them afterwards.
type testServer struct {
	handler  http.Handler
	store    *fakeStore
	blobs    *fakeBlobs
	enqueuer *fakeEnqueuer
}

// newTestServer builds a router over fresh fakes. Each option adjusts the
// dependencies before the router is constructed.
func newTestServer(t *testing.T, opts ...func(*apihttp.Deps)) *testServer {
	t.Helper()

	store := newFakeStore()
	blobs := newFakeBlobs()
	enqueuer := &fakeEnqueuer{}

	deps := apihttp.Deps{
		Config: config.Config{
			Env:    config.EnvDevelopment,
			APIKey: testAPIKey,
			Upload: config.UploadConfig{
				MaxFiles:     3,
				MaxFileBytes: 1024,
			},
		},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:       fakePinger{},
		Store:    store,
		Blobs:    blobs,
		Enqueuer: enqueuer,
	}
	for _, opt := range opts {
		opt(&deps)
	}

	handler, err := apihttp.NewRouter(deps)
	require.NoError(t, err)
	return &testServer{handler: handler, store: store, blobs: blobs, enqueuer: enqueuer}
}

// do issues a request against the router and returns the recorder. It
// supplies the api-key header unless the caller already set one, so the
// tests that are about something else are not all rewritten to carry a
// credential. Use doRaw to exercise the absence of the header.
func (s *testServer) do(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if req.Header.Get(apihttp.APIKeyHeader) == "" {
		req.Header.Set(apihttp.APIKeyHeader, testAPIKey)
	}
	return s.doRaw(t, req)
}

// doRaw issues a request exactly as given, adding no headers.
func (s *testServer) doRaw(t *testing.T, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

// decodeProblem asserts the response is a problem document and returns it.
func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) apihttp.Problem {
	t.Helper()
	require.Contains(t, rec.Header().Get("Content-Type"), apihttp.ProblemContentType)

	var problem apihttp.Problem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &problem))
	assert.Equal(t, rec.Code, problem.Status, "the body status should match the response status")
	assert.NotEmpty(t, problem.Title)
	return problem
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
			server := newTestServer(t, func(d *apihttp.Deps) { d.DB = tc.pinger })

			rec := server.do(t, httptest.NewRequestWithContext(t.Context(), tc.method, tc.target, nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
			assert.Contains(t, rec.Header().Get("Content-Type"), tc.wantJSONType)

			if tc.wantJSONType != apihttp.ProblemContentType {
				var body apihttp.HealthResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, "ok", body.Status)
				return
			}

			problem := decodeProblem(t, rec)
			assert.Equal(t, tc.target, problem.Instance)
			assert.NotEmpty(t, problem.RequestID, "the request id middleware should populate every problem")
		})
	}
}

// TestRecovererReturnsProblem proves a panicking handler still produces a
// well-formed RFC 7807 response rather than a dropped connection.
func TestRecovererReturnsProblem(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	router, ok := server.handler.(interface {
		Get(pattern string, h http.HandlerFunc)
	})
	require.True(t, ok, "the router should expose chi's Get for this test")
	router.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("handler exploded") })

	rec := server.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	problem := decodeProblem(t, rec)
	// The panic value must not reach the client.
	assert.NotContains(t, problem.Detail, "exploded")
}

func TestNewRouterRequiresDependencies(t *testing.T) {
	t.Parallel()

	full := func() apihttp.Deps {
		return apihttp.Deps{
			Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
			DB:       fakePinger{},
			Store:    newFakeStore(),
			Blobs:    newFakeBlobs(),
			Enqueuer: &fakeEnqueuer{},
		}
	}

	tests := []struct {
		name    string
		omit    func(*apihttp.Deps)
		wantMsg string
	}{
		{name: "logger", omit: func(d *apihttp.Deps) { d.Logger = nil }, wantMsg: "Deps.Logger"},
		{name: "database", omit: func(d *apihttp.Deps) { d.DB = nil }, wantMsg: "Deps.DB"},
		{name: "store", omit: func(d *apihttp.Deps) { d.Store = nil }, wantMsg: "Deps.Store"},
		{name: "blobs", omit: func(d *apihttp.Deps) { d.Blobs = nil }, wantMsg: "Deps.Blobs"},
		{name: "enqueuer", omit: func(d *apihttp.Deps) { d.Enqueuer = nil }, wantMsg: "Deps.Enqueuer"},
	}
	for _, tc := range tests {
		t.Run("missing "+tc.name, func(t *testing.T) {
			t.Parallel()
			deps := full()
			tc.omit(&deps)

			_, err := apihttp.NewRouter(deps)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

// TestSwaggerIsDevelopmentOnly proves the generated document is not served
// from a production deployment.
func TestSwaggerIsDevelopmentOnly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        string
		wantStatus int
	}{
		{name: "served outside production", env: config.EnvDevelopment, wantStatus: http.StatusOK},
		{name: "absent in production", env: config.EnvProduction, wantStatus: http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t, func(d *apihttp.Deps) { d.Config.Env = tc.env })

			rec := server.do(t, httptest.NewRequestWithContext(
				t.Context(), http.MethodGet, "/swagger/doc.json", nil))

			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}
