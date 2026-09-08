-- name: CreateTransactions :copyfrom
-- Bulk-loaded rather than inserted one at a time: a year of statements is
-- easily a few thousand rows, and COPY moves them in a single round trip
-- inside the analyze step's transaction.
INSERT INTO transactions (
    analysis_id, txn_date, description, counterparty, amount,
    direction, balance_after, category, essential, recurring
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);
