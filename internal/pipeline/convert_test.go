package pipeline

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
)

func number(s string) json.Number { return json.Number(s) }

func numberPtr(s string) *json.Number {
	n := json.Number(s)
	return &n
}

func TestBuildTransaction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		in            llm.Transaction
		wantDirection string
		wantAmount    string
		wantBalance   bool
		wantErr       string
	}{
		{
			name:          "a plain credit",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("3000.00"), Direction: "credit"},
			wantDirection: db.DirectionCredit,
			wantAmount:    "3000.00",
		},
		{
			name:          "a plain debit",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("42.75"), Direction: "debit"},
			wantDirection: db.DirectionDebit,
			wantAmount:    "42.75",
		},
		{
			name:          "direction wording is normalised",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("10.00"), Direction: "  OUTFLOW "},
			wantDirection: db.DirectionDebit,
			wantAmount:    "10.00",
		},
		{
			name:          "income is a credit",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("10.00"), Direction: "Income"},
			wantDirection: db.DirectionCredit,
			wantAmount:    "10.00",
		},
		{
			// The sign carries the direction, and the magnitude is what
			// gets stored, so the aggregations never have to use abs().
			name:          "a negative amount becomes a positive debit",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("-42.75"), Direction: "unknown wording"},
			wantDirection: db.DirectionDebit,
			wantAmount:    "42.75",
		},
		{
			name:          "an explicit direction wins over the sign",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("-42.75"), Direction: "credit"},
			wantDirection: db.DirectionCredit,
			wantAmount:    "42.75",
		},
		{
			name:          "a leading plus is stripped",
			in:            llm.Transaction{Date: "2025-01-05", Amount: number("+42.75"), Direction: "credit"},
			wantDirection: db.DirectionCredit,
			wantAmount:    "42.75",
		},
		{
			name: "a balance is optional",
			in: llm.Transaction{Date: "2025-01-05", Amount: number("1.00"), Direction: "debit",
				BalanceAfter: numberPtr("2957.25")},
			wantDirection: db.DirectionDebit,
			wantAmount:    "1.00",
			wantBalance:   true,
		},
		{
			name:    "a non-ISO date is rejected",
			in:      llm.Transaction{Date: "05/01/2025", Amount: number("1.00"), Direction: "debit"},
			wantErr: "not in YYYY-MM-DD form",
		},
		{
			name:    "a missing date is rejected",
			in:      llm.Transaction{Amount: number("1.00"), Direction: "debit"},
			wantErr: "not in YYYY-MM-DD form",
		},
		{
			name:    "a non-numeric amount is rejected",
			in:      llm.Transaction{Date: "2025-01-05", Amount: number("about ten"), Direction: "debit"},
			wantErr: "is not a number",
		},
		{
			// Silently guessing would make the row vanish from every
			// chart, since the aggregations filter on credit/debit.
			name:    "an unrecognised direction with no sign is rejected",
			in:      llm.Transaction{Date: "2025-01-05", Amount: number("10.00"), Direction: "sideways"},
			wantErr: "not recognised as credit or debit",
		},
		{
			name:    "a missing direction with no sign is rejected",
			in:      llm.Transaction{Date: "2025-01-05", Amount: number("10.00")},
			wantErr: "direction is missing",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row, err := buildTransaction(tc.in)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDirection, row.Direction)
			assert.True(t, row.TxnDate.Valid)
			assert.Equal(t, tc.wantBalance, row.BalanceAfter.Valid)

			// The decimal must survive unrounded.
			value, err := row.Amount.Value()
			require.NoError(t, err)
			assert.Equal(t, tc.wantAmount, value)
		})
	}
}

func TestBuildAnalysis(t *testing.T) {
	t.Parallel()

	uploadID := uuid.New()
	raw := json.RawMessage(`{"analysis":{},"chart_data":{},"transactions":[]}`)

	resp := &llm.Response{
		Analysis: llm.Analysis{
			Currency:    " EUR ",
			PeriodStart: "2025-01-01",
			PeriodEnd:   "not a date",
			KeyInsights: json.RawMessage(`["stable"]`),
			Anomalies:   json.RawMessage(`null`),
		},
		ChartData: llm.ChartData{Forecast: json.RawMessage(`{"a":1}`)},
		Transactions: []llm.Transaction{
			{Date: "2025-01-05", Amount: number("1.00"), Direction: "debit"},
		},
		Raw: raw,
	}

	out, err := buildAnalysis(uploadID, resp)
	require.NoError(t, err)

	assert.Equal(t, uploadID, out.Analysis.UploadID)
	assert.Equal(t, "EUR", out.Analysis.Currency, "the currency should be trimmed")
	assert.NotEqual(t, uuid.Nil, out.Analysis.ID)

	assert.True(t, out.Analysis.PeriodStart.Valid)
	assert.False(t, out.Analysis.PeriodEnd.Valid,
		"an unparseable period bound is left NULL rather than failing the run")

	assert.JSONEq(t, `["stable"]`, string(out.Analysis.KeyInsights))
	assert.Nil(t, out.Analysis.Anomalies, `a JSON null is stored as SQL NULL, not as the text "null"`)
	assert.Nil(t, out.Analysis.ChartPresent, "an absent section stays NULL")
	assert.JSONEq(t, `{"a":1}`, string(out.Analysis.ChartForecast))
	assert.JSONEq(t, string(raw), string(out.Analysis.RawLlmJson))

	require.Len(t, out.Transactions, 1)
}

// TestBuildAnalysisNamesTheOffendingRow keeps a bad row diagnosable in a
// statement with hundreds of them.
func TestBuildAnalysisNamesTheOffendingRow(t *testing.T) {
	t.Parallel()

	resp := &llm.Response{
		Analysis: llm.Analysis{Currency: "EUR"},
		Transactions: []llm.Transaction{
			{Date: "2025-01-05", Amount: number("1.00"), Direction: "debit"},
			{Date: "2025-01-06", Amount: number("2.00"), Direction: "debit"},
			{Date: "nonsense", Amount: number("3.00"), Direction: "debit"},
		},
	}

	_, err := buildAnalysis(uuid.New(), resp)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "transactions[2]")
}

func TestScrubber(t *testing.T) {
	t.Parallel()

	const key = "sk-secret-key-do-not-leak-0123456789"
	const dsn = "postgres://user:hunter2pass@db:5432/app"

	tests := []struct {
		name        string
		secrets     []string
		err         error
		wantContain string
		wantAbsent  []string
	}{
		{
			name:        "a secret is redacted",
			secrets:     []string{key},
			err:         errors.New("upstream rejected key " + key),
			wantContain: "[redacted]",
			wantAbsent:  []string{key},
		},
		{
			name:        "several secrets are redacted",
			secrets:     []string{key, dsn},
			err:         errors.New("dial " + dsn + " failed while using " + key),
			wantContain: "[redacted]",
			wantAbsent:  []string{key, dsn, "hunter2pass"},
		},
		{
			name:        "newlines are folded so one error stays one log line",
			secrets:     nil,
			err:         errors.New("first line\nsecond line\n\tindented"),
			wantContain: "first line second line indented",
		},
		{
			name:        "a nil error still reports something",
			secrets:     nil,
			err:         nil,
			wantContain: "without reporting a reason",
		},
		{
			// A short value would match everywhere and redact the whole
			// message, and is not a credential worth protecting.
			name:        "trivially short secrets are ignored",
			secrets:     []string{"a"},
			err:         errors.New("a database failure"),
			wantContain: "a database failure",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := newScrubber(tc.secrets).message(tc.err)

			assert.Contains(t, got, tc.wantContain)
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

func TestScrubberTruncatesLongMessages(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 5000)
	got := newScrubber(nil).message(errors.New(long))

	assert.LessOrEqual(t, len(got), maxErrorMessageBytes)
	assert.True(t, strings.HasSuffix(got, "..."))
}

// TestTruncateKeepsRunesIntact proves a multi-byte character is not cut in
// half, which would make the stored reason invalid UTF-8.
func TestTruncateKeepsRunesIntact(t *testing.T) {
	t.Parallel()

	for limit := 4; limit < 40; limit++ {
		got := truncate(strings.Repeat("€", 30), limit)
		assert.True(t, len(got) <= limit, "limit %d produced %d bytes", limit, len(got))
		assert.True(t, isValidUTF8(got), "limit %d split a rune: %q", limit, got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
