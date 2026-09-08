package llm

import (
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

const validReply = `{
  "analysis": {"currency": "EUR", "period_start": "2025-01-01", "period_end": "2025-03-31"},
  "savings_plan": {"target": 500},
  "chart_data": {"present": {}, "forecast": {}},
  "transactions": [
    {"date": "2025-01-05", "description": "Salary", "amount": 3000.00, "direction": "credit"}
  ]
}`

// newTestClient points a client at a stub server standing in for the
// deployment.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := NewClient(config.AzureOpenAI{
		Endpoint:   server.URL,
		APIKey:     "test-key",
		Deployment: "gpt-4o",
		APIVersion: "2024-10-21",
		Timeout:    5 * time.Second,
	}, "you are a financial analyst")
	require.NoError(t, err)
	return client, server
}

func chatReply(content string) string {
	body, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": content}},
		},
	})
	return string(body)
}

// TestAnalyzeSendsTheExpectedRequest pins the contract with the
// deployment: the JSON response format and a temperature of zero are what
// make the reply parseable and reproducible.
func TestAnalyzeSendsTheExpectedRequest(t *testing.T) {
	t.Parallel()

	var (
		gotPath   string
		gotQuery  string
		gotAPIKey string
		gotBody   map[string]any
	)

	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("api-key")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, chatReply(validReply))
	})

	_, err := client.Analyze(t.Context(), []Statement{
		{Filename: "january.pdf", Markdown: "# January"},
		{Filename: "february.pdf", Markdown: "# February"},
	})
	require.NoError(t, err)

	assert.Equal(t, "/openai/deployments/gpt-4o/chat/completions", gotPath)
	assert.Equal(t, "api-version=2024-10-21", gotQuery)
	assert.Equal(t, "test-key", gotAPIKey)

	assert.Equal(t, float64(0), gotBody["temperature"], "extraction must be deterministic")
	assert.Equal(t, map[string]any{"type": "json_object"}, gotBody["response_format"])

	messages, ok := gotBody["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 2)

	system := messages[0].(map[string]any)
	assert.Equal(t, "system", system["role"])
	assert.Equal(t, "you are a financial analyst", system["content"])

	user := messages[1].(map[string]any)
	assert.Equal(t, "user", user["role"])
	content, _ := user["content"].(string)
	assert.Contains(t, content, "--- STATEMENT 1 ---\n# January")
	assert.Contains(t, content, "--- STATEMENT 2 ---\n# February")
}

func TestAnalyzeDecodesAValidReply(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, chatReply(validReply))
	})

	resp, err := client.Analyze(t.Context(), []Statement{{Markdown: "# January"}})
	require.NoError(t, err)

	assert.Equal(t, "EUR", resp.Analysis.Currency)
	require.Len(t, resp.Transactions, 1)
	assert.Equal(t, "3000.00", resp.Transactions[0].Amount.String(),
		"the decimal should survive as written rather than through a float")
	assert.JSONEq(t, validReply, string(resp.Raw))
}

func TestAnalyzeRejectsUnusableReplies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "not JSON at all",
			content: "I'm sorry, I can't help with that.",
			wantErr: "not a JSON object",
		},
		{
			name:    "truncated JSON",
			content: `{"analysis": {"currency": "EUR"`,
			wantErr: "not a JSON object",
		},
		{
			name:    "a JSON array rather than an object",
			content: `[1, 2, 3]`,
			wantErr: "not a JSON object",
		},
		{
			name:    "missing every required key",
			content: `{"something_else": true}`,
			wantErr: "missing the required top-level keys [analysis chart_data transactions]",
		},
		{
			name:    "missing one required key",
			content: `{"analysis": {"currency": "EUR"}, "transactions": []}`,
			wantErr: "missing the required top-level keys [chart_data]",
		},
		{
			name:    "an empty currency",
			content: `{"analysis": {"currency": ""}, "chart_data": {}, "transactions": []}`,
			wantErr: "analysis.currency is empty",
		},
		{
			name:    "a wrongly typed member",
			content: `{"analysis": {"currency": 42}, "chart_data": {}, "transactions": []}`,
			wantErr: "could not be decoded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, chatReply(tc.content))
			})

			_, err := client.Analyze(t.Context(), []Statement{{Markdown: "x"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)

			var invalid *InvalidResponseError
			require.ErrorAs(t, err, &invalid)
			assert.False(t, invalid.Retryable(),
				"the same prompt yields the same shape, so retrying is pointless")
		})
	}
}

// TestAnalyzeAcceptsUnknownSections proves the prompt can grow new sections
// without this code having to be updated in lockstep.
func TestAnalyzeAcceptsUnknownSections(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, chatReply(`{
		  "analysis": {"currency": "GBP"},
		  "chart_data": {},
		  "transactions": [],
		  "a_section_added_later": {"anything": true}
		}`))
	})

	resp, err := client.Analyze(t.Context(), []Statement{{Markdown: "x"}})
	require.NoError(t, err)
	assert.Equal(t, "GBP", resp.Analysis.Currency)
}

func TestAnalyzeRejectsATruncatedReply(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{
				{"finish_reason": "length", "message": map[string]string{"content": `{"analysis":`}},
			},
		})
		_, _ = w.Write(body)
	})

	_, err := client.Analyze(t.Context(), []Statement{{Markdown: "x"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated by the model's output limit")
}

func TestAnalyzeClassifiesUpstreamFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        int
		wantRetryable bool
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, wantRetryable: true},
		{name: "server error", status: http.StatusInternalServerError, wantRetryable: true},
		{name: "bad gateway", status: http.StatusBadGateway, wantRetryable: true},
		{name: "bad request", status: http.StatusBadRequest, wantRetryable: false},
		{name: "unauthorised", status: http.StatusUnauthorized, wantRetryable: false},
		{name: "content filtered", status: http.StatusUnprocessableEntity, wantRetryable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream said no"}}`)
			})

			_, err := client.Analyze(t.Context(), []Statement{{Markdown: "x"}})
			require.Error(t, err)

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tc.status, apiErr.StatusCode)
			assert.Equal(t, tc.wantRetryable, apiErr.Retryable())
			assert.Contains(t, err.Error(), "upstream said no")
		})
	}
}

func TestNewClientValidatesItsInputs(t *testing.T) {
	t.Parallel()

	valid := config.AzureOpenAI{Endpoint: "https://example.com", Deployment: "d", APIVersion: "v"}

	_, err := NewClient(config.AzureOpenAI{Endpoint: "  "}, "prompt")
	require.Error(t, err)

	_, err = NewClient(valid, "   ")
	require.Error(t, err, "an empty system prompt would silently produce nonsense")

	client, err := NewClient(valid, "prompt")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(client.url, "https://example.com/openai/deployments/d/"))
}

func TestAnalyzeRefusesZeroStatements(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) {
		t.Error("no request should be made for zero statements")
	})

	_, err := client.Analyze(t.Context(), nil)
	require.Error(t, err)
}
