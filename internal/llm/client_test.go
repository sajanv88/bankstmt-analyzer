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
			name:    "no currency anywhere",
			content: `{"analysis": {"currency": ""}, "chart_data": {}, "transactions": []}`,
			wantErr: "no currency was reported",
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

// promptShapedReply is the output structure that prompts/analysis_system.md
// actually asks the model for, trimmed to one entry per array. It exists so
// a change to either the prompt or these structs cannot silently stop the
// other from working: this is the contract between them.
const promptShapedReply = `{
  "meta": {
    "currency": "EUR",
    "period_covered": {"start": "2025-01-01", "end": "2025-03-31"},
    "statements_processed": 3,
    "extraction_issues": [],
    "reconciliation": [{"statement_period": "2025-01", "expected_delta": 100, "actual_delta": 100, "matches": true}]
  },
  "accounts": [{"bank": "Example Bank", "holder": "A Person", "account_masked": "****1234",
                "period_start": "2025-01-01", "period_end": "2025-01-31",
                "opening_balance": 1000, "closing_balance": 1100}],
  "transactions": [
    {"date": "2025-01-05", "description": "SALARY", "counterparty": "ACME",
     "amount": 3000, "direction": "credit", "balance_after": 4000, "type": null,
     "category": "income", "essential": false, "recurring": true},
    {"date": "2025-01-09", "description": "SUPERMARKET", "counterparty": "Shop",
     "amount": 42.75, "direction": "debit", "balance_after": null, "type": "card",
     "category": "groceries", "essential": true, "recurring": false}
  ],
  "analysis": {
    "monthly_summary": [{"month": "2025-01", "income": 3000, "spending": 42.75, "net": 2957.25,
                         "savings_rate_pct": 98.6, "essential": 42.75, "discretionary": 0}],
    "category_totals": [{"category": "groceries", "total": 42.75, "pct_of_spending": 100,
                         "monthly_avg": 14.25, "trend": "stable"}],
    "recurring_payments": [{"counterparty": "ACME", "amount": 3000, "cadence": "monthly",
                            "category": "income", "flag": null}],
    "top_merchants": [{"merchant": "Shop", "total": 42.75, "count": 1}],
    "anomalies": [],
    "key_insights": ["Spending is stable"]
  },
  "savings_plan": {
    "target_monthly_savings": 500, "target_savings_rate_pct": 16.7,
    "projected_3_month_savings": 1500,
    "category_budgets": [{"category": "groceries", "current_monthly_avg": 14.25,
                          "proposed_cap": 12, "monthly_saving": 2.25}],
    "recommended_cuts": [{"rank": 1, "action": "Cancel unused subscription", "category": "subscriptions",
                          "estimated_monthly_saving": 9.99, "evidence": "charged monthly", "effort": "low"}],
    "quick_wins": [], "habits": [], "risks": []
  },
  "chart_data": {
    "present": {
      "monthly_income_vs_spending": {"labels": ["2025-01"], "income": [3000], "spending": [42.75], "net": [2957.25]},
      "category_breakdown": {"labels": ["groceries"], "values": [42.75]},
      "monthly_by_category": {"labels": ["2025-01"], "series": [{"category": "groceries", "values": [42.75]}]},
      "essential_vs_discretionary": {"labels": ["2025-01"], "essential": [42.75], "discretionary": [0]},
      "balance_over_time": {"dates": ["2025-01-05"], "balances": [4000]}
    },
    "forecast": {
      "labels": ["2025-04"], "baseline_spending": [42.75], "planned_spending": [40],
      "baseline_savings": [500], "planned_savings": [520],
      "cumulative_savings_baseline": [500], "cumulative_savings_planned": [520],
      "category_budgets": {"labels": ["groceries"], "current": [14.25], "proposed": [12]},
      "assumptions": ["income stays flat"]
    }
  },
  "disclaimer": "Budgeting guidance, not financial advice."
}`

func TestAnalyzeAcceptsThePromptsOutputShape(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, chatReply(promptShapedReply))
	})

	resp, err := client.Analyze(t.Context(), []Statement{{Markdown: "# January"}})
	require.NoError(t, err)

	// The prompt reports these under meta, not under analysis.
	assert.Equal(t, "EUR", resp.Currency())
	assert.Equal(t, "2025-01-01", resp.PeriodStart())
	assert.Equal(t, "2025-03-31", resp.PeriodEnd())
	assert.Empty(t, resp.Analysis.Currency, "the shipped prompt puts currency in meta")

	require.Len(t, resp.Transactions, 2)
	assert.Equal(t, "credit", resp.Transactions[0].Direction)
	assert.Equal(t, "3000", resp.Transactions[0].Amount.String())
	assert.Nil(t, resp.Transactions[1].BalanceAfter, "a null balance stays absent")
	assert.Equal(t, "groceries", resp.Transactions[1].Category)
	assert.True(t, resp.Transactions[1].Essential)

	// The sections that become jsonb columns are carried through.
	assert.NotEmpty(t, resp.Analysis.MonthlySummary)
	assert.NotEmpty(t, resp.Analysis.CategoryTotals)
	assert.NotEmpty(t, resp.Analysis.RecurringPayments)
	assert.NotEmpty(t, resp.Analysis.KeyInsights)
	assert.NotEmpty(t, resp.SavingsPlan)
	assert.NotEmpty(t, resp.ChartData.Present)
	assert.NotEmpty(t, resp.ChartData.Forecast)
}

// TestCurrencyFallsBackToAnalysis covers a prompt that reports the currency
// the older way, so moving it back would not break the pipeline.
func TestCurrencyFallsBackToAnalysis(t *testing.T) {
	t.Parallel()

	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, chatReply(`{
		  "analysis": {"currency": "GBP", "period_start": "2025-02-01", "period_end": "2025-02-28"},
		  "chart_data": {}, "transactions": []
		}`))
	})

	resp, err := client.Analyze(t.Context(), []Statement{{Markdown: "x"}})
	require.NoError(t, err)
	assert.Equal(t, "GBP", resp.Currency())
	assert.Equal(t, "2025-02-01", resp.PeriodStart())
}
