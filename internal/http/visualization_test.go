package http_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	apihttp "github.com/sajanv88/bankstmt-analyzer/internal/http"
)

func date(y int, m time.Month, d int) pgtype.Date {
	return pgtype.Date{Time: time.Date(y, m, d, 0, 0, 0, 0, time.UTC), Valid: true}
}

// seedCompleted wires up a completed upload whose transactions run through
// March 2025, and returns its upload id.
func seedCompleted(t *testing.T, server *testServer) uuid.UUID {
	t.Helper()

	uploadID := uuid.New()
	analysisID := uuid.New()
	server.store.putUpload(db.Upload{ID: uploadID, Status: db.StatusCompleted})
	server.store.putAnalysis(uploadID, db.Analysis{
		ID:       analysisID,
		UploadID: uploadID,
		Currency: "EUR",
		// A sentinel the handler must never echo: `present` is required to
		// be recomputed from the transactions table for the window asked
		// for, not served from this column.
		ChartPresent:  []byte(`{"sentinel":"stored-chart-present"}`),
		ChartForecast: []byte(`{"projected_savings":[{"month":"2025-04","amount":812.5}]}`),
	})
	server.store.bounds = db.GetTransactionMonthBoundsRow{
		FirstMonth: date(2024, time.September, 1),
		LastMonth:  date(2025, time.March, 1),
	}
	return uploadID
}

func getVisualization(t *testing.T, server *testServer, id uuid.UUID, query string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/uploads/" + id.String() + "/visualization"
	if query != "" {
		target += "?" + query
	}
	return server.do(t, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
}

// TestVisualizationWindowSelection pins the window the handler asks the
// database for, which is the whole contract of the months/from/to
// parameters.
func TestVisualizationWindowSelection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantFrom  time.Time
		wantTo    time.Time
		wantPerio apihttp.Period
	}{
		{
			name:      "default is the last three months with data",
			query:     "",
			wantFrom:  time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2025, time.March, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2025-01", To: "2025-03"},
		},
		{
			name:      "months=1 is the last month with data",
			query:     "months=1",
			wantFrom:  time.Date(2025, time.March, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2025, time.March, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2025-03", To: "2025-03"},
		},
		{
			name:      "months=12 counts a full year back",
			query:     "months=12",
			wantFrom:  time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2025, time.March, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-04", To: "2025-03"},
		},
		{
			name:      "from and to override months entirely",
			query:     "months=12&from=2024-11&to=2024-12",
			wantFrom:  time.Date(2024, time.November, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2024, time.December, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-11", To: "2024-12"},
		},
		{
			name:      "from alone runs to the last month with data",
			query:     "from=2024-12",
			wantFrom:  time.Date(2024, time.December, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2025, time.March, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-12", To: "2025-03"},
		},
		{
			name:      "to alone counts months back from it",
			query:     "to=2024-12&months=2",
			wantFrom:  time.Date(2024, time.November, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2024, time.December, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-11", To: "2024-12"},
		},
		{
			name:      "a window spanning a year boundary",
			query:     "from=2024-12&to=2025-01",
			wantFrom:  time.Date(2024, time.December, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2025, time.January, 31, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-12", To: "2025-01"},
		},
		{
			name:      "February gets its real last day",
			query:     "from=2024-02&to=2024-02",
			wantFrom:  time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC),
			wantTo:    time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
			wantPerio: apihttp.Period{From: "2024-02", To: "2024-02"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t)
			id := seedCompleted(t, server)

			rec := getVisualization(t, server, id, tc.query)
			require.Equal(t, http.StatusOK, rec.Code)

			windows := server.store.windowsRequested()
			require.Len(t, windows, 1)
			assert.Equal(t, tc.wantFrom, windows[0].PeriodStart.Time, "period start")
			assert.Equal(t, tc.wantTo, windows[0].PeriodEnd.Time, "period end")

			var resp apihttp.VisualizationResponse
			require.NoError(t, decodeJSON(t, rec, &resp))
			assert.Equal(t, tc.wantPerio, resp.Period, "the response should echo the window it used")
		})
	}
}

func TestVisualizationRejectsInvalidWindows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantParam string
	}{
		{name: "months below the range", query: "months=0", wantParam: "months"},
		{name: "months above the range", query: "months=13", wantParam: "months"},
		{name: "months not a number", query: "months=three", wantParam: "months"},
		{name: "months negative", query: "months=-2", wantParam: "months"},
		{name: "from is not a month", query: "from=2025-13", wantParam: "from"},
		{name: "from is a full date", query: "from=2025-01-15", wantParam: "from"},
		{name: "to is malformed", query: "to=nonsense", wantParam: "to"},
		{name: "from later than to", query: "from=2025-03&to=2025-01", wantParam: "from"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t)
			id := seedCompleted(t, server)

			rec := getVisualization(t, server, id, tc.query)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			problem := decodeProblem(t, rec)
			require.NotEmpty(t, problem.InvalidParams)
			assert.Equal(t, tc.wantParam, problem.InvalidParams[0].Name)
			assert.Empty(t, server.store.windowsRequested(), "an invalid window must not reach the database")
		})
	}
}

func TestVisualizationRequiresCompletedUpload(t *testing.T) {
	t.Parallel()

	for _, status := range []string{db.StatusPending, db.StatusProcessing, db.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			server := newTestServer(t)
			id := uuid.New()
			server.store.putUpload(db.Upload{ID: id, Status: status})

			rec := getVisualization(t, server, id, "")

			require.Equal(t, http.StatusConflict, rec.Code)
			assert.Contains(t, decodeProblem(t, rec).Detail, status)
		})
	}
}

func TestVisualizationUnknownUpload(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	rec := getVisualization(t, server, uuid.New(), "")

	require.Equal(t, http.StatusNotFound, rec.Code)
	decodeProblem(t, rec)
}

// TestVisualizationRecomputesPresent proves the present series come from
// the transaction aggregations rather than from the stored chart_present.
func TestVisualizationRecomputesPresent(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	id := seedCompleted(t, server)

	server.store.income = []db.MonthlyIncomeVsSpendingRow{
		{Month: "2025-01", Income: 3200, Spending: 2400},
		{Month: "2025-02", Income: 3200, Spending: 2800},
	}
	server.store.categories = []db.CategoryBreakdownRow{
		{Category: "Groceries", Amount: 1200, TransactionCount: 40},
		{Category: "Transport", Amount: 400, TransactionCount: 12},
	}
	server.store.monthCategories = []db.MonthlyByCategoryRow{
		{Month: "2025-01", Category: "Groceries", Amount: 600},
	}
	server.store.splits = []db.EssentialVsDiscretionaryRow{
		{Month: "2025-01", Essential: 1800, Discretionary: 600},
	}
	server.store.balances = []db.BalanceOverTimeRow{
		{TxnDate: date(2025, time.January, 31), Balance: 4210.88},
	}
	server.store.summary = db.WindowSummaryRow{
		TotalIncome: 6400, TotalSpending: 5200,
		EssentialSpending: 3600, RecurringSpending: 1500,
		TransactionCount: 120, MonthCount: 2,
	}

	rec := getVisualization(t, server, id, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "stored-chart-present",
		"present must be rebuilt from transactions, not echoed from chart_present")

	var resp apihttp.VisualizationResponse
	require.NoError(t, decodeJSON(t, rec, &resp))

	assert.Equal(t, "EUR", resp.Currency)

	// Net is derived per month.
	require.Len(t, resp.Present.MonthlyIncomeVsSpending, 2)
	assert.InDelta(t, 800.0, resp.Present.MonthlyIncomeVsSpending[0].Net, 0.001)
	assert.InDelta(t, 400.0, resp.Present.MonthlyIncomeVsSpending[1].Net, 0.001)

	// Share is each category against total window spending.
	require.Len(t, resp.Present.CategoryBreakdown, 2)
	assert.InDelta(t, 1200.0/5200.0, resp.Present.CategoryBreakdown[0].Share, 0.0001)
	assert.InDelta(t, 400.0/5200.0, resp.Present.CategoryBreakdown[1].Share, 0.0001)

	assert.Equal(t, "2025-01-31", resp.Present.BalanceOverTime[0].Date)
	assert.InDelta(t, 4210.88, resp.Present.BalanceOverTime[0].Balance, 0.001)

	// Summary derivations.
	assert.InDelta(t, 1200.0, resp.Summary.NetSavings, 0.001)
	assert.InDelta(t, 1200.0/6400.0, resp.Summary.SavingsRate, 0.0001)
	assert.InDelta(t, 1600.0, resp.Summary.DiscretionarySpending, 0.001)
	assert.InDelta(t, 3200.0, resp.Summary.AverageMonthlyIncome, 0.001)
	assert.InDelta(t, 2600.0, resp.Summary.AverageMonthlySpending, 0.001)

	// Forecast is served verbatim from the stored column.
	assert.JSONEq(t, `{"projected_savings":[{"month":"2025-04","amount":812.5}]}`, string(resp.Forecast))
}

// TestVisualizationEmptyWindow proves an empty window answers with empty
// series and zeroed derivations rather than nulls or NaN.
func TestVisualizationEmptyWindow(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	id := seedCompleted(t, server)
	// Leave every canned result empty and the analysis without a forecast.
	server.store.putAnalysis(id, db.Analysis{ID: uuid.New(), UploadID: id, Currency: "GBP"})

	rec := getVisualization(t, server, id, "from=2030-01&to=2030-01")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, `"monthly_income_vs_spending":[]`, "an empty series must marshal as [] not null")
	assert.Contains(t, body, `"balance_over_time":[]`)
	assert.Contains(t, body, `"forecast":null`)

	var resp apihttp.VisualizationResponse
	require.NoError(t, decodeJSON(t, rec, &resp))
	assert.Zero(t, resp.Summary.SavingsRate, "a zero denominator must yield 0, not NaN")
	assert.Zero(t, resp.Summary.AverageMonthlyIncome)
	assert.Empty(t, resp.Present.CategoryBreakdown)
}

// TestVisualizationWithNoTransactionsStillAnswers proves an analysis with
// no rows at all produces a coherent window rather than a zero date.
func TestVisualizationWithNoTransactionsStillAnswers(t *testing.T) {
	t.Parallel()

	server := newTestServer(t)
	uploadID := uuid.New()
	server.store.putUpload(db.Upload{ID: uploadID, Status: db.StatusCompleted})
	server.store.putAnalysis(uploadID, db.Analysis{
		ID:        uuid.New(),
		UploadID:  uploadID,
		Currency:  "USD",
		PeriodEnd: date(2025, time.June, 30),
	})
	// bounds stays zero: no transactions were recorded.

	rec := getVisualization(t, server, uploadID, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var resp apihttp.VisualizationResponse
	require.NoError(t, decodeJSON(t, rec, &resp))
	// Falls back to the analysis period rather than to a zero time.
	assert.Equal(t, apihttp.Period{From: "2025-04", To: "2025-06"}, resp.Period)
}
