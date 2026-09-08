-- name: GetAnalysisByUpload :one
SELECT * FROM analyses WHERE upload_id = sqlc.arg(upload_id);

-- name: GetAnalysisIDByUpload :one
SELECT id FROM analyses WHERE upload_id = sqlc.arg(upload_id);

-- name: CreateAnalysis :one
INSERT INTO analyses (
    id, upload_id, currency, period_start, period_end,
    monthly_summary, category_totals, recurring_payments, anomalies,
    key_insights, savings_plan, chart_present, chart_forecast, raw_llm_json
) VALUES (
    sqlc.arg(id),
    sqlc.arg(upload_id),
    sqlc.arg(currency),
    sqlc.narg(period_start),
    sqlc.narg(period_end),
    sqlc.narg(monthly_summary),
    sqlc.narg(category_totals),
    sqlc.narg(recurring_payments),
    sqlc.narg(anomalies),
    sqlc.narg(key_insights),
    sqlc.narg(savings_plan),
    sqlc.narg(chart_present),
    sqlc.narg(chart_forecast),
    sqlc.narg(raw_llm_json)
)
RETURNING *;

-- name: DeleteAnalysisByUpload :execrows
-- Transactions cascade from the analysis, so this is the whole of the
-- analyze step's compensation.
DELETE FROM analyses WHERE upload_id = sqlc.arg(upload_id);
