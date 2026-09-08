package db

// Transaction direction values. The LLM's vocabulary is not fixed, so the
// pipeline normalises whatever it returns onto exactly these two before
// insert. The visualization queries filter on them directly, which is why
// they live here rather than being spelled out at each call site.
const (
	DirectionCredit = "credit"
	DirectionDebit  = "debit"
)

// Upload status values, matching the CHECK constraint on uploads.status.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
)

// Amount convention
//
// transactions.amount is stored as a positive magnitude; the sign lives in
// direction. Summing debits therefore needs no abs(), and a mis-signed row
// cannot silently cancel out a correct one in an aggregate. The pipeline
// enforces this on the way in.
//
// Money convention
//
// The visualization queries cast money to float8 on the way out. Those are
// display quantities for a chart, not ledger figures: statement-scale
// values stay well inside the range float64 represents exactly at two
// decimal places, and threading pgtype.Numeric through the DTOs would buy
// precision that nothing downstream can spend. Anything that must stay
// exact — the stored rows themselves — remains numeric(14,2) in Postgres.
