package pipeline

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/sajanv88/bankstmt-analyzer/internal/db"
	"github.com/sajanv88/bankstmt-analyzer/internal/llm"
)

// dateLayout is the only date format accepted from the model. The prompt
// asks for ISO dates; accepting more formats here would mean guessing
// whether 03/04 is March or April, which is not a guess worth making about
// somebody's bank statement.
const dateLayout = "2006-01-02"

// directionSynonyms maps what a model might plausibly write onto the two
// values the schema and the visualization queries use.
var directionSynonyms = map[string]string{
	db.DirectionCredit: db.DirectionCredit,
	"credits":          db.DirectionCredit,
	"in":               db.DirectionCredit,
	"inflow":           db.DirectionCredit,
	"income":           db.DirectionCredit,
	"deposit":          db.DirectionCredit,
	db.DirectionDebit:  db.DirectionDebit,
	"debits":           db.DirectionDebit,
	"out":              db.DirectionDebit,
	"outflow":          db.DirectionDebit,
	"expense":          db.DirectionDebit,
	"spending":         db.DirectionDebit,
	"payment":          db.DirectionDebit,
	"withdraw":         db.DirectionDebit,
}

// buildAnalysis converts the model's reply into the rows to be written.
func buildAnalysis(uploadID uuid.UUID, resp *llm.Response) (db.NewAnalysis, error) {
	if resp == nil {
		return db.NewAnalysis{}, fmt.Errorf("the model returned no analysis")
	}

	analysis := db.CreateAnalysisParams{
		ID:                uuid.New(),
		UploadID:          uploadID,
		Currency:          strings.TrimSpace(resp.Analysis.Currency),
		PeriodStart:       optionalDate(resp.Analysis.PeriodStart),
		PeriodEnd:         optionalDate(resp.Analysis.PeriodEnd),
		MonthlySummary:    jsonOrNil(resp.Analysis.MonthlySummary),
		CategoryTotals:    jsonOrNil(resp.Analysis.CategoryTotals),
		RecurringPayments: jsonOrNil(resp.Analysis.RecurringPayments),
		Anomalies:         jsonOrNil(resp.Analysis.Anomalies),
		KeyInsights:       jsonOrNil(resp.Analysis.KeyInsights),
		SavingsPlan:       jsonOrNil(resp.SavingsPlan),
		ChartPresent:      jsonOrNil(resp.ChartData.Present),
		ChartForecast:     jsonOrNil(resp.ChartData.Forecast),
		RawLlmJson:        jsonOrNil(resp.Raw),
	}

	transactions := make([]db.CreateTransactionsParams, 0, len(resp.Transactions))
	for i, txn := range resp.Transactions {
		row, err := buildTransaction(txn)
		if err != nil {
			return db.NewAnalysis{}, fmt.Errorf("transactions[%d]: %w", i, err)
		}
		transactions = append(transactions, row)
	}

	return db.NewAnalysis{Analysis: analysis, Transactions: transactions}, nil
}

// buildTransaction converts one entry of the transactions array.
func buildTransaction(txn llm.Transaction) (db.CreateTransactionsParams, error) {
	when, err := time.Parse(dateLayout, strings.TrimSpace(txn.Date))
	if err != nil {
		return db.CreateTransactionsParams{}, fmt.Errorf("date %q is not in YYYY-MM-DD form", txn.Date)
	}

	magnitude, negative := splitSign(txn.Amount.String())
	amount, err := numeric(magnitude)
	if err != nil {
		return db.CreateTransactionsParams{}, fmt.Errorf("amount %q is not a number", txn.Amount.String())
	}

	direction, err := resolveDirection(txn.Direction, negative)
	if err != nil {
		return db.CreateTransactionsParams{}, err
	}

	balance, err := optionalNumeric(txn.BalanceAfter)
	if err != nil {
		return db.CreateTransactionsParams{}, fmt.Errorf("balance_after is not a number: %w", err)
	}

	return db.CreateTransactionsParams{
		TxnDate:      pgtype.Date{Time: when, Valid: true},
		Description:  strings.TrimSpace(txn.Description),
		Counterparty: strings.TrimSpace(txn.Counterparty),
		Amount:       amount,
		Direction:    direction,
		BalanceAfter: balance,
		Category:     strings.TrimSpace(txn.Category),
		Essential:    txn.Essential,
		Recurring:    txn.Recurring,
	}, nil
}

// resolveDirection normalises the model's wording onto credit or debit.
//
// A negative amount is the fallback signal when the wording is not
// recognised, since a statement that signs its amounts is expressing the
// same thing. An unrecognised direction with no sign to fall back on is an
// error rather than a guess: the visualization queries filter on exactly
// these two values, so a third one would quietly vanish from every chart.
func resolveDirection(raw string, negative bool) (string, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if direction, ok := directionSynonyms[key]; ok {
		return direction, nil
	}
	if negative {
		return db.DirectionDebit, nil
	}
	if key == "" {
		return "", fmt.Errorf("direction is missing and the amount is not signed")
	}
	return "", fmt.Errorf("direction %q is not recognised as credit or debit", raw)
}

// splitSign separates a decimal's sign from its magnitude, because amounts
// are stored as positive magnitudes with the sign carried by direction.
func splitSign(amount string) (magnitude string, negative bool) {
	amount = strings.TrimSpace(amount)
	switch {
	case strings.HasPrefix(amount, "-"):
		return strings.TrimSpace(amount[1:]), true
	case strings.HasPrefix(amount, "+"):
		return strings.TrimSpace(amount[1:]), false
	default:
		return amount, false
	}
}

// numeric parses a decimal string into a Postgres numeric without going
// through float64, so the value written is exactly the value the model
// wrote.
func numeric(value string) (pgtype.Numeric, error) {
	var n pgtype.Numeric
	if strings.TrimSpace(value) == "" {
		return n, fmt.Errorf("empty value")
	}
	if err := n.Scan(value); err != nil {
		return pgtype.Numeric{}, fmt.Errorf("parse %q: %w", value, err)
	}
	return n, nil
}

func optionalNumeric(value *json.Number) (pgtype.Numeric, error) {
	if value == nil || strings.TrimSpace(value.String()) == "" {
		return pgtype.Numeric{}, nil
	}
	return numeric(value.String())
}

// optionalDate parses a date the model may have omitted. A missing or
// unparseable period bound is left NULL rather than failing the run: the
// visualization endpoint derives its window from the transactions
// themselves and only falls back to this.
func optionalDate(value string) pgtype.Date {
	parsed, err := time.Parse(dateLayout, strings.TrimSpace(value))
	if err != nil {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: parsed, Valid: true}
}

// jsonOrNil keeps an absent section NULL rather than storing the literal
// four bytes "null", so an omitted section reads as absent in SQL too.
func jsonOrNil(raw json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return raw
}
