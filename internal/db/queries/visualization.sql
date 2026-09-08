-- Chart aggregations for GET /api/v1/uploads/{id}/visualization.
-- See conventions.go for the amount/direction conventions these depend on
-- and for why money leaves the database as float8.

-- name: GetTransactionMonthBounds :one
-- Bounds the default window. Both columns are NULL when the analysis has
-- no transactions at all, which the handler treats as an empty result
-- rather than an error.
SELECT
    MIN(date_trunc('month', txn_date))::date AS first_month,
    MAX(date_trunc('month', txn_date))::date AS last_month
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id);

-- name: MonthlyIncomeVsSpending :many
SELECT
    to_char(date_trunc('month', txn_date), 'YYYY-MM')                        AS month,
    COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::float8     AS income,
    COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::float8      AS spending
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end)
GROUP BY 1
ORDER BY 1;

-- name: CategoryBreakdown :many
-- Spending only: an income category is not a slice of a spending pie.
SELECT
    COALESCE(NULLIF(category, ''), 'Uncategorised')::text  AS category,
    COALESCE(SUM(amount), 0)::float8                 AS amount,
    COUNT(*)::bigint                                 AS transaction_count
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end)
  AND direction = 'debit'
GROUP BY 1
ORDER BY 2 DESC, 1;

-- name: MonthlyByCategory :many
SELECT
    to_char(date_trunc('month', txn_date), 'YYYY-MM')  AS month,
    COALESCE(NULLIF(category, ''), 'Uncategorised')::text  AS category,
    COALESCE(SUM(amount), 0)::float8                   AS amount
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end)
  AND direction = 'debit'
GROUP BY 1, 2
ORDER BY 1, 3 DESC, 2;

-- name: EssentialVsDiscretionary :many
SELECT
    to_char(date_trunc('month', txn_date), 'YYYY-MM')                  AS month,
    COALESCE(SUM(amount) FILTER (WHERE essential), 0)::float8          AS essential,
    COALESCE(SUM(amount) FILTER (WHERE NOT essential), 0)::float8      AS discretionary
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end)
  AND direction = 'debit'
GROUP BY 1
ORDER BY 1;

-- name: BalanceOverTime :many
-- One point per day: the closing balance, taken as the balance after the
-- last transaction recorded for that day. Rows arrive in statement order,
-- so the highest id on a given date is the latest one.
SELECT DISTINCT ON (txn_date)
    txn_date,
    balance_after::float8 AS balance
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end)
  AND balance_after IS NOT NULL
ORDER BY txn_date, id DESC;

-- name: WindowSummary :one
SELECT
    COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::float8                    AS total_income,
    COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::float8                     AS total_spending,
    COALESCE(SUM(amount) FILTER (WHERE direction = 'debit' AND essential), 0)::float8       AS essential_spending,
    COALESCE(SUM(amount) FILTER (WHERE direction = 'debit' AND recurring), 0)::float8       AS recurring_spending,
    COUNT(*)::bigint                                                                        AS transaction_count,
    COUNT(DISTINCT date_trunc('month', txn_date))::bigint                                   AS month_count
FROM transactions
WHERE analysis_id = sqlc.arg(analysis_id)
  AND txn_date >= sqlc.arg(period_start)
  AND txn_date <= sqlc.arg(period_end);
