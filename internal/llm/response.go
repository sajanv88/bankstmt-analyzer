// Package llm talks to the Azure OpenAI deployment that turns statement
// markdown into the structured analysis the service stores and serves.
package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Response is the model's reply, decoded.
//
// Most sections are kept as raw JSON rather than modelled field by field:
// they are stored in jsonb columns and served back to the client
// unchanged, so decoding them here would only create a second place to
// keep in sync with the prompt. The parts this service actually reasons
// about — the transactions, the currency and the period — are typed.
type Response struct {
	Analysis     Analysis        `json:"analysis"`
	SavingsPlan  json.RawMessage `json:"savings_plan"`
	ChartData    ChartData       `json:"chart_data"`
	Transactions []Transaction   `json:"transactions"`

	// Raw is the model's reply exactly as it arrived, stored in
	// raw_llm_json so a later prompt change can be reconciled against what
	// the model actually said.
	Raw json.RawMessage `json:"-"`
}

// Analysis is the `analysis` object. Its sub-objects map one-to-one onto
// the jsonb columns of the analyses table.
type Analysis struct {
	Currency          string          `json:"currency"`
	PeriodStart       string          `json:"period_start"`
	PeriodEnd         string          `json:"period_end"`
	MonthlySummary    json.RawMessage `json:"monthly_summary"`
	CategoryTotals    json.RawMessage `json:"category_totals"`
	RecurringPayments json.RawMessage `json:"recurring_payments"`
	Anomalies         json.RawMessage `json:"anomalies"`
	KeyInsights       json.RawMessage `json:"key_insights"`
}

// ChartData is the `chart_data` object: what the model charted for the
// period it saw, and what it projects beyond it.
type ChartData struct {
	Present  json.RawMessage `json:"present"`
	Forecast json.RawMessage `json:"forecast"`
}

// Transaction is one entry of the `transactions` array, flattened into the
// transactions table.
//
// Amount is a json.Number so the decimal the model wrote is stored exactly
// as written; parsing it through float64 first would round it before it
// ever reached a numeric column.
type Transaction struct {
	Date         string       `json:"date"`
	Description  string       `json:"description"`
	Counterparty string       `json:"counterparty"`
	Amount       json.Number  `json:"amount"`
	Direction    string       `json:"direction"`
	BalanceAfter *json.Number `json:"balance_after"`
	Category     string       `json:"category"`
	Essential    bool         `json:"essential"`
	Recurring    bool         `json:"recurring"`
}

// requiredKeys are the top-level members the pipeline cannot proceed
// without. Everything else is optional: a model that omits `anomalies`
// because it found none should not fail the run.
var requiredKeys = []string{"analysis", "chart_data", "transactions"}

// InvalidResponseError means the model replied with something this service
// cannot use. It is not retryable: the same prompt will produce the same
// shape, so spending the retry budget on it only delays the failure.
type InvalidResponseError struct {
	Reason string
}

func (e *InvalidResponseError) Error() string { return "llm: invalid response: " + e.Reason }

// Retryable is always false. See InvalidResponseError.
func (e *InvalidResponseError) Retryable() bool { return false }

// parseResponse decodes and validates the model's JSON reply.
//
// Decoding is deliberately lenient about unknown members — the prompt may
// legitimately grow new sections ahead of this code — but strict about the
// top-level members the pipeline depends on, so a truncated or off-format
// reply fails here rather than as a confusing database error later.
func parseResponse(raw []byte) (*Response, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &InvalidResponseError{Reason: "the reply is not a JSON object: " + err.Error()}
	}

	var missing []string
	for _, key := range requiredKeys {
		if _, ok := envelope[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &InvalidResponseError{
			Reason: fmt.Sprintf("the reply is missing the required top-level keys %v", missing),
		}
	}

	var resp Response
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&resp); err != nil {
		return nil, &InvalidResponseError{Reason: "the reply could not be decoded: " + err.Error()}
	}
	if resp.Analysis.Currency == "" {
		return nil, &InvalidResponseError{Reason: "analysis.currency is empty"}
	}
	resp.Raw = raw
	return &resp, nil
}
