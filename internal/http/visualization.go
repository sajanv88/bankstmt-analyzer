package http

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
)

// dayLayout is how balance_over_time renders its dates.
const dayLayout = "2006-01-02"

type visualizationHandler struct {
	store  UploadStore
	logger *slog.Logger
}

func newVisualizationHandler(store UploadStore, logger *slog.Logger) *visualizationHandler {
	return &visualizationHandler{store: store, logger: logger}
}

// get serves the chart data for a completed upload.
//
//	@Summary		Get visualization data
//	@Description	Returns chart data for a completed upload. The `present` series are recomputed from the recorded transactions for the requested window, so they always match the window asked for; `forecast` is the model's projection, served as stored. The default window is the last 3 months that carry data. `months` (1-12) counts back from the last month with data; `from` and `to` (YYYY-MM) override it, and supplying just one of them anchors that end.
//	@Tags			uploads
//	@Produce		json
//	@Param			id		path		string	true	"Upload id"	format(uuid)
//	@Param			months	query		int		false	"Number of months to include, 1-12"	default(3)
//	@Param			from	query		string	false	"First month, YYYY-MM"
//	@Param			to		query		string	false	"Last month, YYYY-MM"
//	@Success		200		{object}	VisualizationResponse
//	@Failure		400		{object}	Problem	"Invalid id or window parameters"
//	@Failure		404		{object}	Problem	"No such upload"
//	@Failure		409		{object}	Problem	"The upload is not completed"
//	@Failure		500		{object}	Problem
//	@Router			/api/v1/uploads/{id}/visualization [get]
func (h *visualizationHandler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, problem := uploadIDFromRequest(r)
	if problem != nil {
		WriteProblem(w, r, h.logger, problem)
		return
	}

	analysis, problem, err := h.loadCompletedAnalysis(ctx, id)
	if err != nil {
		WriteError(w, r, h.logger, err)
		return
	}
	if problem != nil {
		WriteProblem(w, r, h.logger, problem)
		return
	}

	bounds, err := h.store.GetTransactionMonthBounds(ctx, analysis.ID)
	if err != nil {
		WriteError(w, r, h.logger, fmt.Errorf("load transaction bounds for analysis %s: %w", analysis.ID, err))
		return
	}

	window, problem := resolveWindow(r.URL.Query(), anchorMonth(bounds, analysis))
	if problem != nil {
		WriteProblem(w, r, h.logger, problem)
		return
	}

	resp, err := h.buildResponse(ctx, analysis, window)
	if err != nil {
		WriteError(w, r, h.logger, err)
		return
	}
	WriteJSON(w, r, h.logger, http.StatusOK, resp)
}

// loadCompletedAnalysis fetches the analysis for an upload, rejecting the
// request unless the upload has finished processing. It returns a problem
// for a client-visible refusal and an error for anything else.
func (h *visualizationHandler) loadCompletedAnalysis(ctx context.Context, id uuid.UUID) (db.Analysis, *Problem, error) {
	upload, err := h.store.GetUpload(ctx, id)
	if db.IsNotFound(err) {
		return db.Analysis{}, notFoundProblem(id), nil
	}
	if err != nil {
		return db.Analysis{}, nil, fmt.Errorf("load upload %s: %w", id, err)
	}
	if upload.Status != db.StatusCompleted {
		return db.Analysis{}, NewProblem(http.StatusConflict, fmt.Sprintf(
			"Visualization data is available only once analysis has completed; this upload is %s.",
			upload.Status)), nil
	}

	analysis, err := h.store.GetAnalysisByUpload(ctx, id)
	if db.IsNotFound(err) {
		// A completed upload should always have an analysis. Treat the
		// mismatch as "not ready" rather than as a server fault, since a
		// client retry is the sensible response either way.
		h.logger.WarnContext(ctx, "completed upload has no analysis", "upload_id", id.String())
		return db.Analysis{}, NewProblem(http.StatusConflict,
			"Visualization data is not available for this upload."), nil
	}
	if err != nil {
		return db.Analysis{}, nil, fmt.Errorf("load analysis for upload %s: %w", id, err)
	}
	return analysis, nil, nil
}

// buildResponse runs the window aggregations and assembles the payload.
func (h *visualizationHandler) buildResponse(ctx context.Context, analysis db.Analysis, window monthWindow) (VisualizationResponse, error) {
	start := dateParam(window.periodStart())
	end := dateParam(window.periodEnd())

	summary, err := h.store.WindowSummary(ctx, db.WindowSummaryParams{
		AnalysisID: analysis.ID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return VisualizationResponse{}, fmt.Errorf("summarise window: %w", err)
	}

	present, err := h.buildPresent(ctx, analysis.ID, start, end, summary.TotalSpending)
	if err != nil {
		return VisualizationResponse{}, err
	}

	return VisualizationResponse{
		Currency: analysis.Currency,
		Period:   window.period(),
		Present:  present,
		Forecast: jsonOrNull(analysis.ChartForecast),
		Summary:  buildSummary(summary),
	}, nil
}

func (h *visualizationHandler) buildPresent(
	ctx context.Context,
	analysisID uuid.UUID,
	start, end pgtype.Date,
	totalSpending float64,
) (PresentCharts, error) {
	incomeRows, err := h.store.MonthlyIncomeVsSpending(ctx, db.MonthlyIncomeVsSpendingParams{
		AnalysisID: analysisID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return PresentCharts{}, fmt.Errorf("load monthly income vs spending: %w", err)
	}
	categoryRows, err := h.store.CategoryBreakdown(ctx, db.CategoryBreakdownParams{
		AnalysisID: analysisID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return PresentCharts{}, fmt.Errorf("load category breakdown: %w", err)
	}
	monthCategoryRows, err := h.store.MonthlyByCategory(ctx, db.MonthlyByCategoryParams{
		AnalysisID: analysisID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return PresentCharts{}, fmt.Errorf("load monthly spending by category: %w", err)
	}
	splitRows, err := h.store.EssentialVsDiscretionary(ctx, db.EssentialVsDiscretionaryParams{
		AnalysisID: analysisID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return PresentCharts{}, fmt.Errorf("load essential vs discretionary split: %w", err)
	}
	balanceRows, err := h.store.BalanceOverTime(ctx, db.BalanceOverTimeParams{
		AnalysisID: analysisID, PeriodStart: start, PeriodEnd: end,
	})
	if err != nil {
		return PresentCharts{}, fmt.Errorf("load balance over time: %w", err)
	}

	return PresentCharts{
		MonthlyIncomeVsSpending: mapSlice(incomeRows, func(row db.MonthlyIncomeVsSpendingRow) MonthlyIncomeSpendingPoint {
			return MonthlyIncomeSpendingPoint{
				Month:    row.Month,
				Income:   row.Income,
				Spending: row.Spending,
				Net:      row.Income - row.Spending,
			}
		}),
		CategoryBreakdown: mapSlice(categoryRows, func(row db.CategoryBreakdownRow) CategorySlice {
			return CategorySlice{
				Category:         row.Category,
				Amount:           row.Amount,
				TransactionCount: row.TransactionCount,
				Share:            ratio(row.Amount, totalSpending),
			}
		}),
		MonthlyByCategory: mapSlice(monthCategoryRows, func(row db.MonthlyByCategoryRow) MonthCategoryPoint {
			return MonthCategoryPoint{Month: row.Month, Category: row.Category, Amount: row.Amount}
		}),
		EssentialVsDiscretionary: mapSlice(splitRows, func(row db.EssentialVsDiscretionaryRow) EssentialSplitPoint {
			return EssentialSplitPoint{
				Month:         row.Month,
				Essential:     row.Essential,
				Discretionary: row.Discretionary,
			}
		}),
		BalanceOverTime: mapSlice(balanceRows, func(row db.BalanceOverTimeRow) BalancePoint {
			return BalancePoint{Date: row.TxnDate.Time.Format(dayLayout), Balance: row.Balance}
		}),
	}, nil
}

func buildSummary(row db.WindowSummaryRow) VisualizationSummary {
	net := row.TotalIncome - row.TotalSpending
	return VisualizationSummary{
		TotalIncome:            row.TotalIncome,
		TotalSpending:          row.TotalSpending,
		NetSavings:             net,
		SavingsRate:            ratio(net, row.TotalIncome),
		EssentialSpending:      row.EssentialSpending,
		DiscretionarySpending:  row.TotalSpending - row.EssentialSpending,
		RecurringSpending:      row.RecurringSpending,
		AverageMonthlyIncome:   ratio(row.TotalIncome, float64(row.MonthCount)),
		AverageMonthlySpending: ratio(row.TotalSpending, float64(row.MonthCount)),
		TransactionCount:       row.TransactionCount,
		MonthCount:             row.MonthCount,
	}
}

// anchorMonth is the month the default window counts back from: the last
// month carrying a transaction, falling back to the analysis period and
// then to the current month so an analysis with no rows still answers with
// a coherent window rather than a zero date.
func anchorMonth(bounds db.GetTransactionMonthBoundsRow, analysis db.Analysis) time.Time {
	if bounds.LastMonth.Valid {
		return bounds.LastMonth.Time
	}
	if analysis.PeriodEnd.Valid {
		return analysis.PeriodEnd.Time
	}
	return time.Now().UTC()
}

// jsonOrNull renders a jsonb column for embedding in the response. A NULL
// column arrives as a nil slice, which would marshal as an invalid empty
// value inside json.RawMessage.
func jsonOrNull(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// ratio divides safely, returning 0 rather than NaN or Inf for a zero
// denominator, because a chart cannot render either.
func ratio(numerator, denominator float64) float64 {
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

func dateParam(t time.Time) pgtype.Date {
	return pgtype.Date{Time: t, Valid: true}
}

// mapSlice converts query rows to DTOs, always returning a non-nil slice so
// an empty series marshals as [] rather than null.
func mapSlice[In, Out any](rows []In, fn func(In) Out) []Out {
	out := make([]Out, 0, len(rows))
	for _, row := range rows {
		out = append(out, fn(row))
	}
	return out
}
