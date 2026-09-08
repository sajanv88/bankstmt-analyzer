package db_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/migrate"
)

// These tests exercise the visualization SQL against a real Postgres,
// because that is the only place the window arithmetic, the direction
// filters and the DISTINCT ON in BalanceOverTime can actually be verified.
// They are skipped unless DATABASE_URL points at a database the test may
// migrate and write to; CI supplies a service container.

func requireStore(t *testing.T) (*db.Store, *pgxpool.Pool) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set; skipping the Postgres integration test")
	}

	ctx := t.Context()
	pool, err := db.NewPool(ctx, dsn, db.PoolConfig{MaxConns: 4})
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	require.NoError(t, migrate.Up(ctx, pool, slog.New(slog.NewJSONHandler(io.Discard, nil))))
	return db.NewStore(pool), pool
}

// txn is one row of test data.
type txn struct {
	date         string
	amount       string
	direction    string
	category     string
	essential    bool
	recurring    bool
	balanceAfter string
}

// seedAnalysis inserts an upload, an analysis and the given transactions,
// and registers cleanup. It writes with plain SQL rather than through the
// generated queries so the fixture stays independent of the insert paths
// the pipeline will use.
func seedAnalysis(t *testing.T, pool *pgxpool.Pool, rows []txn) uuid.UUID {
	t.Helper()

	ctx := t.Context()
	uploadID, analysisID := uuid.New(), uuid.New()

	_, err := pool.Exec(ctx,
		`INSERT INTO uploads (id, status, file_count) VALUES ($1, $2, 1)`,
		uploadID, db.StatusCompleted)
	require.NoError(t, err)

	t.Cleanup(func() {
		// The analysis and transactions cascade from the upload.
		_, err := pool.Exec(context.Background(), `DELETE FROM uploads WHERE id = $1`, uploadID)
		assert.NoError(t, err)
	})

	_, err = pool.Exec(ctx,
		`INSERT INTO analyses (id, upload_id, currency, period_start, period_end)
		 VALUES ($1, $2, 'EUR', '2024-11-01', '2025-03-31')`,
		analysisID, uploadID)
	require.NoError(t, err)

	for i, row := range rows {
		_, err = pool.Exec(ctx,
			`INSERT INTO transactions
			 (analysis_id, txn_date, description, counterparty, amount, direction,
			  balance_after, category, essential, recurring)
			 VALUES ($1, $2, $3, '', $4::numeric, $5, NULLIF($6, '')::numeric, $7, $8, $9)`,
			analysisID, row.date, "row", row.amount, row.direction,
			row.balanceAfter, row.category, row.essential, row.recurring)
		require.NoError(t, err, "seeding row %d", i)
	}
	return analysisID
}

func day(s string) pgtype.Date {
	parsed, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return pgtype.Date{Time: parsed, Valid: true}
}

// sampleRows spans November 2024 to March 2025 so a three-month window
// necessarily has to exclude something.
func sampleRows() []txn {
	return []txn{
		// Outside a Jan-Mar window: proves the window actually filters.
		{date: "2024-11-15", amount: "999.00", direction: db.DirectionDebit, category: "Groceries", balanceAfter: "100.00"},
		{date: "2024-12-15", amount: "888.00", direction: db.DirectionCredit, category: "Salary", balanceAfter: "200.00"},

		{date: "2025-01-01", amount: "3000.00", direction: db.DirectionCredit, category: "Salary", recurring: true, balanceAfter: "3000.00"},
		{date: "2025-01-10", amount: "200.00", direction: db.DirectionDebit, category: "Groceries", essential: true, balanceAfter: "2800.00"},
		{date: "2025-01-10", amount: "50.00", direction: db.DirectionDebit, category: "Dining", balanceAfter: "2750.00"},
		{date: "2025-01-20", amount: "800.00", direction: db.DirectionDebit, category: "Rent", essential: true, recurring: true, balanceAfter: "1950.00"},

		{date: "2025-02-01", amount: "3000.00", direction: db.DirectionCredit, category: "Salary", recurring: true, balanceAfter: "4950.00"},
		{date: "2025-02-14", amount: "150.00", direction: db.DirectionDebit, category: "Dining", balanceAfter: "4800.00"},
		{date: "2025-02-20", amount: "800.00", direction: db.DirectionDebit, category: "Rent", essential: true, recurring: true, balanceAfter: "4000.00"},

		{date: "2025-03-01", amount: "3000.00", direction: db.DirectionCredit, category: "Salary", recurring: true, balanceAfter: "7000.00"},
		{date: "2025-03-05", amount: "300.00", direction: db.DirectionDebit, category: "Groceries", essential: true, balanceAfter: "6700.00"},
		// No category: must be bucketed as Uncategorised.
		{date: "2025-03-09", amount: "25.00", direction: db.DirectionDebit, category: "", balanceAfter: "6675.00"},
	}
}

func TestMonthBoundsAndWindowFiltering(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, sampleRows())
	ctx := t.Context()

	bounds, err := store.GetTransactionMonthBounds(ctx, analysisID)
	require.NoError(t, err)
	require.True(t, bounds.FirstMonth.Valid)
	require.True(t, bounds.LastMonth.Valid)
	assert.Equal(t, day("2024-11-01").Time, bounds.FirstMonth.Time)
	assert.Equal(t, day("2025-03-01").Time, bounds.LastMonth.Time,
		"the anchor for the default window is the last month with data")

	// The default three-month window ending at the anchor.
	rows, err := store.MonthlyIncomeVsSpending(ctx, db.MonthlyIncomeVsSpendingParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-03-31"),
	})
	require.NoError(t, err)
	require.Len(t, rows, 3, "November and December fall outside the window")

	assert.Equal(t, "2025-01", rows[0].Month)
	assert.InDelta(t, 3000.0, rows[0].Income, 0.001)
	assert.InDelta(t, 1050.0, rows[0].Spending, 0.001)

	assert.Equal(t, "2025-02", rows[1].Month)
	assert.InDelta(t, 950.0, rows[1].Spending, 0.001)

	assert.Equal(t, "2025-03", rows[2].Month)
	assert.InDelta(t, 325.0, rows[2].Spending, 0.001)
}

// TestWindowBoundariesAreInclusive proves a transaction on the first or
// last day of the window is counted, which is what the month arithmetic in
// the handler relies on.
func TestWindowBoundariesAreInclusive(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, []txn{
		{date: "2025-01-01", amount: "10.00", direction: db.DirectionDebit, category: "Edge"},
		{date: "2025-01-31", amount: "20.00", direction: db.DirectionDebit, category: "Edge"},
		{date: "2024-12-31", amount: "40.00", direction: db.DirectionDebit, category: "Outside"},
		{date: "2025-02-01", amount: "80.00", direction: db.DirectionDebit, category: "Outside"},
	})

	summary, err := store.WindowSummary(t.Context(), db.WindowSummaryParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-01-31"),
	})
	require.NoError(t, err)
	assert.InDelta(t, 30.0, summary.TotalSpending, 0.001, "both boundary days count, neither neighbour does")
	assert.Equal(t, int64(2), summary.TransactionCount)
	assert.Equal(t, int64(1), summary.MonthCount)
}

func TestCategoryBreakdownCountsSpendingOnly(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, sampleRows())

	rows, err := store.CategoryBreakdown(t.Context(), db.CategoryBreakdownParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-03-31"),
	})
	require.NoError(t, err)

	byCategory := map[string]db.CategoryBreakdownRow{}
	for _, row := range rows {
		byCategory[row.Category] = row
	}

	assert.NotContains(t, byCategory, "Salary", "income is not a slice of a spending breakdown")
	assert.InDelta(t, 1600.0, byCategory["Rent"].Amount, 0.001)
	assert.InDelta(t, 500.0, byCategory["Groceries"].Amount, 0.001)
	assert.Equal(t, int64(2), byCategory["Groceries"].TransactionCount)
	assert.InDelta(t, 25.0, byCategory["Uncategorised"].Amount, 0.001,
		"a blank category is bucketed rather than dropped")

	// Ordered by amount descending, so the biggest slice leads.
	require.NotEmpty(t, rows)
	assert.Equal(t, "Rent", rows[0].Category)
}

func TestEssentialSplitAndSummaryDerivations(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, sampleRows())
	ctx := t.Context()

	params := db.EssentialVsDiscretionaryParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-03-31"),
	}
	splits, err := store.EssentialVsDiscretionary(ctx, params)
	require.NoError(t, err)
	require.Len(t, splits, 3)

	assert.Equal(t, "2025-01", splits[0].Month)
	assert.InDelta(t, 1000.0, splits[0].Essential, 0.001, "groceries plus rent")
	assert.InDelta(t, 50.0, splits[0].Discretionary, 0.001, "dining")

	summary, err := store.WindowSummary(ctx, db.WindowSummaryParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-03-31"),
	})
	require.NoError(t, err)
	assert.InDelta(t, 9000.0, summary.TotalIncome, 0.001)
	assert.InDelta(t, 2325.0, summary.TotalSpending, 0.001)
	assert.InDelta(t, 2100.0, summary.EssentialSpending, 0.001)
	assert.InDelta(t, 1600.0, summary.RecurringSpending, 0.001, "recurring credits are not recurring spending")
	assert.Equal(t, int64(3), summary.MonthCount)
}

// TestBalanceOverTimeTakesTheLastBalancePerDay covers the DISTINCT ON: two
// transactions share 2025-01-10, and the closing balance is the later one.
func TestBalanceOverTimeTakesTheLastBalancePerDay(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, sampleRows())

	rows, err := store.BalanceOverTime(t.Context(), db.BalanceOverTimeParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2025-01-01"),
		PeriodEnd:   day("2025-01-31"),
	})
	require.NoError(t, err)
	require.Len(t, rows, 3, "one point per day with a balance, not one per transaction")

	assert.Equal(t, day("2025-01-10").Time, rows[1].TxnDate.Time)
	assert.InDelta(t, 2750.0, rows[1].Balance, 0.001,
		"the closing balance is the one after the last transaction of that day")

	// Ascending by date, which is what a time series needs.
	assert.Equal(t, day("2025-01-01").Time, rows[0].TxnDate.Time)
	assert.Equal(t, day("2025-01-20").Time, rows[2].TxnDate.Time)
}

func TestEmptyWindowReturnsZeroesNotNulls(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, sampleRows())
	ctx := t.Context()

	empty := db.WindowSummaryParams{
		AnalysisID:  analysisID,
		PeriodStart: day("2030-01-01"),
		PeriodEnd:   day("2030-01-31"),
	}
	summary, err := store.WindowSummary(ctx, empty)
	require.NoError(t, err, "COALESCE should keep an empty window scannable")
	assert.Zero(t, summary.TotalIncome)
	assert.Zero(t, summary.TotalSpending)
	assert.Equal(t, int64(0), summary.TransactionCount)

	rows, err := store.MonthlyIncomeVsSpending(ctx, db.MonthlyIncomeVsSpendingParams{
		AnalysisID:  analysisID,
		PeriodStart: empty.PeriodStart,
		PeriodEnd:   empty.PeriodEnd,
	})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

// TestMonthBoundsWithNoTransactions proves the bounds query stays scannable
// for an analysis that has no rows, which is what the handler's fallback
// depends on.
func TestMonthBoundsWithNoTransactions(t *testing.T) {
	store, pool := requireStore(t)
	analysisID := seedAnalysis(t, pool, nil)

	bounds, err := store.GetTransactionMonthBounds(t.Context(), analysisID)
	require.NoError(t, err)
	assert.False(t, bounds.FirstMonth.Valid, "MIN over no rows is NULL")
	assert.False(t, bounds.LastMonth.Valid)
}
