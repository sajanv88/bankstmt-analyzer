package ocr

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewClient(config.AzureOCR{
		Endpoint: server.URL,
		APIKey:   "test-key",
		Model:    "mistral-ocr-2503",
		Path:     "/providers/mistral/azure/ocr",
		Timeout:  5 * time.Second,
	})
	require.NoError(t, err)
	return client
}

// TestExtractSendsTheDocumentInline pins how the PDF is delivered: the
// files live in this service's blob store, which the OCR service has no
// route to, so the bytes have to travel in the request itself.
func TestExtractSendsTheDocumentInline(t *testing.T) {
	t.Parallel()

	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
	)

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		_, _ = io.WriteString(w, `{"pages":[{"index":0,"markdown":"# Page one"}]}`)
	})

	_, err := client.Extract(t.Context(), []byte("%PDF-1.7 body"))
	require.NoError(t, err)

	assert.Equal(t, "/providers/mistral/azure/ocr", gotPath)
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "mistral-ocr-2503", gotBody["model"])
	assert.Equal(t, false, gotBody["include_image_base64"])

	document, ok := gotBody["document"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "document_url", document["type"])

	url, _ := document["document_url"].(string)
	const prefix = "data:application/pdf;base64,"
	require.True(t, strings.HasPrefix(url, prefix), "got %q", url)

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(url, prefix))
	require.NoError(t, err)
	assert.Equal(t, "%PDF-1.7 body", string(decoded))
}

func TestExtractReturnsPages(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pages":[
		  {"index":0,"markdown":"# Page one  "},
		  {"index":1,"markdown":"  # Page two"}
		]}`)
	})

	result, err := client.Extract(t.Context(), []byte("%PDF-1.7"))
	require.NoError(t, err)

	assert.Equal(t, 2, result.PageCount())
	// Pages are joined with a rule so the model can still see where each
	// page began.
	assert.Equal(t, "# Page one\n\n---\n\n# Page two", result.Markdown())
}

func TestExtractRejectsAnEmptyResult(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"pages":[]}`)
	})

	_, err := client.Extract(t.Context(), []byte("%PDF-1.7"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pages")
}

func TestExtractClassifiesUpstreamFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		wantRetryable bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantRetryable: true},
		{name: "server error", status: http.StatusInternalServerError, wantRetryable: true},
		{name: "service unavailable", status: http.StatusServiceUnavailable, wantRetryable: true},
		{name: "bad request", status: http.StatusBadRequest, wantRetryable: false},
		{name: "unauthorised", status: http.StatusUnauthorized, wantRetryable: false},
		{name: "payload too large", status: http.StatusRequestEntityTooLarge, wantRetryable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"detail":"the document could not be processed"}`)
			})

			_, err := client.Extract(t.Context(), []byte("%PDF-1.7"))
			require.Error(t, err)

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tc.status, apiErr.StatusCode)
			assert.Equal(t, tc.wantRetryable, apiErr.Retryable())
			assert.Equal(t, 30*time.Second, apiErr.RetryAfter)
			assert.Contains(t, err.Error(), "could not be processed")
		})
	}
}

// TestExtractTruncatesAHugeErrorBody stops an upstream fault from pasting a
// whole document into a log line.
func TestExtractTruncatesAHugeErrorBody(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, strings.Repeat("x", 100_000))
	})

	_, err := client.Extract(t.Context(), []byte("%PDF-1.7"))
	require.Error(t, err)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.LessOrEqual(t, len(apiErr.Body), maxErrorBodyBytes)
}

func TestExtractTreatsATransportFailureAsRetryable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client, err := NewClient(config.AzureOCR{
		Endpoint: server.URL,
		APIKey:   "k",
		Model:    "m",
		Path:     "/ocr",
		Timeout:  time.Second,
	})
	require.NoError(t, err)
	// Closing the server makes the request fail before it is answered.
	server.Close()

	_, err = client.Extract(t.Context(), []byte("%PDF-1.7"))
	require.Error(t, err)

	var transportErr *TransportError
	require.ErrorAs(t, err, &transportErr)
	assert.True(t, transportErr.Retryable(), "the request never reached the service")
}

func TestExtractRefusesAnEmptyDocument(t *testing.T) {
	t.Parallel()

	client := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no request should be made for an empty document")
	})

	_, err := client.Extract(t.Context(), nil)
	require.Error(t, err)
}

func TestMarkdownOfNoPagesIsEmpty(t *testing.T) {
	t.Parallel()
	assert.Empty(t, Result{}.Markdown())
	assert.Zero(t, Result{}.PageCount())
}
