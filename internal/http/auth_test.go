package http_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
)

// protectedRoutes is every endpoint the api-key guard covers. Listing them
// here rather than testing one and assuming the rest means a route added
// outside the guarded subtree is caught by the "all of them" assertions
// below, not by whoever deploys it.
func protectedRoutes(id uuid.UUID) []struct {
	name   string
	method string
	target string
} {
	return []struct {
		name   string
		method string
		target string
	}{
		{"create upload", http.MethodPost, "/api/v1/uploads"},
		{"upload status", http.MethodGet, "/api/v1/uploads/" + id.String() + "/status"},
		{"visualization", http.MethodGet, "/api/v1/uploads/" + id.String() + "/visualization"},
	}
}

func TestProtectedRoutesRejectRequestsWithoutAValidKey(t *testing.T) {
	t.Parallel()

	keys := []struct {
		name   string
		header string
		set    bool
	}{
		{name: "no header at all"},
		{name: "empty header", header: "", set: true},
		{name: "wrong key of the right shape", header: strings.Repeat("a", config.APIKeyLength), set: true},
		{name: "correct key with one character changed", header: "0123456789abcdef0123456789abcdee", set: true},
		{name: "correct key truncated", header: "0123456789abcdef", set: true},
		{name: "correct key with trailing whitespace", header: testAPIKey + " ", set: true},
	}

	for _, route := range protectedRoutes(uuid.New()) {
		for _, key := range keys {
			t.Run(route.name+"/"+key.name, func(t *testing.T) {
				t.Parallel()

				server := newTestServer(t)
				req := httptest.NewRequestWithContext(t.Context(), route.method, route.target, nil)
				if key.set {
					req.Header.Set(apihttp.APIKeyHeader, key.header)
				}

				rec := server.doRaw(t, req)

				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				problem := decodeProblem(t, rec)
				assert.Equal(t, http.StatusUnauthorized, problem.Status)
				// The rejection must not reveal which part was wrong, and
				// must never echo the configured key back.
				assert.NotContains(t, rec.Body.String(), testAPIKey)
			})
		}
	}
}

func TestProtectedRoutesAcceptTheConfiguredKey(t *testing.T) {
	t.Parallel()

	// A 404 or a 400 from the handler is a pass here: it means the request
	// reached the handler, which is all this test is about. Only 401 is a
	// failure.
	for _, route := range protectedRoutes(uuid.New()) {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()

			server := newTestServer(t)
			req := httptest.NewRequestWithContext(t.Context(), route.method, route.target, nil)
			req.Header.Set(apihttp.APIKeyHeader, testAPIKey)

			rec := server.doRaw(t, req)

			assert.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"the configured key should reach the handler")
		})
	}
}

// The header name is matched case-insensitively, because Go canonicalises
// header keys. Callers write api-key, Api-Key or API-KEY and all three work.
func TestAPIKeyHeaderIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"api-key", "Api-Key", "API-KEY"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := newTestServer(t)
			req := httptest.NewRequestWithContext(t.Context(),
				http.MethodGet, "/api/v1/uploads/"+uuid.New().String()+"/status", nil)
			req.Header.Set(name, testAPIKey)

			rec := server.doRaw(t, req)

			assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

// The probes are what kubelet calls, and it cannot present a credential.
func TestProbesAndSwaggerStayOpen(t *testing.T) {
	t.Parallel()

	for _, target := range []string{"/healthz", "/readyz", "/swagger/doc.json"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()

			server := newTestServer(t)
			rec := server.doRaw(t,
				httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))

			assert.NotEqual(t, http.StatusUnauthorized, rec.Code)
		})
	}
}

// A router built without a key would serve an unauthenticated API, so it is
// refused outright rather than defaulting to open.
func TestNewRouterRequiresAnAPIKey(t *testing.T) {
	t.Parallel()

	_, err := apihttp.NewRouter(apihttp.Deps{
		Config:   config.Config{Env: config.EnvDevelopment},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:       fakePinger{},
		Store:    newFakeStore(),
		Blobs:    newFakeBlobs(),
		Enqueuer: &fakeEnqueuer{},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "APIKey")
}

// The preflight has to advertise the header, or a browser blocks the real
// request before the server ever sees it.
func TestCORSPreflightAllowsTheAPIKeyHeader(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	req := httptest.NewRequestWithContext(t.Context(),
		http.MethodOptions, "/api/v1/uploads", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", apihttp.APIKeyHeader)

	rec := server.doRaw(t, req)

	allowed := rec.Header().Get("Access-Control-Allow-Headers")
	assert.Contains(t, strings.ToLower(allowed), apihttp.APIKeyHeader)
}
