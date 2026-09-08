package http

import (
	"encoding/json"
	"time"
)

// CreateUploadResponse is returned by POST /api/v1/uploads.
type CreateUploadResponse struct {
	ID     string `json:"id" example:"7c9e6679-7425-40de-944b-e07fc1f90ae7"`
	Status string `json:"status" example:"pending"`
}

// UploadStatusResponse is returned by GET /api/v1/uploads/{id}/status.
//
// FailureReason appears only for a failed upload and AnalysisID only for a
// completed one, so a client can tell the terminal states apart without
// consulting the status string twice.
type UploadStatusResponse struct {
	ID            string    `json:"id" example:"7c9e6679-7425-40de-944b-e07fc1f90ae7"`
	Status        string    `json:"status" enums:"pending,processing,completed,failed"`
	FailureReason string    `json:"failure_reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	AnalysisID    string    `json:"analysis_id,omitempty"`
}

// VisualizationResponse is returned by
// GET /api/v1/uploads/{id}/visualization.
type VisualizationResponse struct {
	Currency string        `json:"currency" example:"EUR"`
	Period   Period        `json:"period"`
	Present  PresentCharts `json:"present"`
	// Forecast is served verbatim from the analysis's chart_forecast
	// column: it is the model's projection, not something that can be
	// recomputed from recorded transactions. It is null when the model
	// produced no forecast.
	Forecast json.RawMessage      `json:"forecast" swaggertype:"object"`
	Summary  VisualizationSummary `json:"summary"`
}

// Period is the requested window, inclusive at both ends, as YYYY-MM.
type Period struct {
	From string `json:"from" example:"2025-01"`
	To   string `json:"to" example:"2025-03"`
}

// PresentCharts holds the five series recomputed from the transactions
// table for the requested window.
type PresentCharts struct {
	MonthlyIncomeVsSpending  []MonthlyIncomeSpendingPoint `json:"monthly_income_vs_spending"`
	CategoryBreakdown        []CategorySlice              `json:"category_breakdown"`
	MonthlyByCategory        []MonthCategoryPoint         `json:"monthly_by_category"`
	EssentialVsDiscretionary []EssentialSplitPoint        `json:"essential_vs_discretionary"`
	BalanceOverTime          []BalancePoint               `json:"balance_over_time"`
}

// MonthlyIncomeSpendingPoint is one month of income against spending.
type MonthlyIncomeSpendingPoint struct {
	Month    string  `json:"month" example:"2025-01"`
	Income   float64 `json:"income" example:"3200.00"`
	Spending float64 `json:"spending" example:"2410.55"`
	// Net is income minus spending: positive means the month added to
	// savings.
	Net float64 `json:"net" example:"789.45"`
}

// CategorySlice is one category's share of spending across the window.
type CategorySlice struct {
	Category         string  `json:"category" example:"Groceries"`
	Amount           float64 `json:"amount" example:"612.30"`
	TransactionCount int64   `json:"transaction_count" example:"27"`
	// Share is the fraction of total window spending, 0..1.
	Share float64 `json:"share" example:"0.18"`
}

// MonthCategoryPoint is one month's spending in one category.
type MonthCategoryPoint struct {
	Month    string  `json:"month" example:"2025-01"`
	Category string  `json:"category" example:"Groceries"`
	Amount   float64 `json:"amount" example:"204.10"`
}

// EssentialSplitPoint splits one month's spending into essential and
// discretionary.
type EssentialSplitPoint struct {
	Month         string  `json:"month" example:"2025-01"`
	Essential     float64 `json:"essential" example:"1580.00"`
	Discretionary float64 `json:"discretionary" example:"830.55"`
}

// BalancePoint is the closing balance on one day.
type BalancePoint struct {
	Date    string  `json:"date" example:"2025-01-31"`
	Balance float64 `json:"balance" example:"4210.88"`
}

// VisualizationSummary aggregates the whole requested window.
type VisualizationSummary struct {
	TotalIncome   float64 `json:"total_income" example:"9600.00"`
	TotalSpending float64 `json:"total_spending" example:"7231.65"`
	NetSavings    float64 `json:"net_savings" example:"2368.35"`
	// SavingsRate is NetSavings over TotalIncome, 0 when there is no
	// income in the window.
	SavingsRate            float64 `json:"savings_rate" example:"0.2467"`
	EssentialSpending      float64 `json:"essential_spending" example:"4740.00"`
	DiscretionarySpending  float64 `json:"discretionary_spending" example:"2491.65"`
	RecurringSpending      float64 `json:"recurring_spending" example:"1830.00"`
	AverageMonthlyIncome   float64 `json:"average_monthly_income" example:"3200.00"`
	AverageMonthlySpending float64 `json:"average_monthly_spending" example:"2410.55"`
	TransactionCount       int64   `json:"transaction_count" example:"184"`
	// MonthCount is how many distinct months actually carry data in the
	// window, which can be fewer than the months the window spans.
	MonthCount int64 `json:"month_count" example:"3"`
}
