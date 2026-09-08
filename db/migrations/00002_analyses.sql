-- +goose Up
-- One analysis per upload. The jsonb columns mirror the corresponding
-- objects of the LLM response verbatim; they are nullable rather than
-- defaulted because the model decides which sections it emits, and an
-- absent section should read as absent rather than as an empty object.
CREATE TABLE analyses (
    id                 uuid PRIMARY KEY,
    upload_id          uuid NOT NULL UNIQUE REFERENCES uploads (id) ON DELETE CASCADE,
    currency           text NOT NULL,
    period_start       date,
    period_end         date,
    monthly_summary    jsonb,
    category_totals    jsonb,
    recurring_payments jsonb,
    anomalies          jsonb,
    key_insights       jsonb,
    savings_plan       jsonb,
    chart_present      jsonb,
    chart_forecast     jsonb,
    raw_llm_json       jsonb,
    created_at         timestamptz NOT NULL DEFAULT now()
);

-- The flattened transactions[] array of the LLM response. direction and
-- category carry no CHECK constraint on purpose: the vocabulary comes from
-- the model, and a constraint violation here would fail the whole pipeline
-- over a synonym. Normalisation happens in Go before insert instead.
CREATE TABLE transactions (
    id            bigserial PRIMARY KEY,
    analysis_id   uuid NOT NULL REFERENCES analyses (id) ON DELETE CASCADE,
    txn_date      date NOT NULL,
    description   text NOT NULL,
    counterparty  text NOT NULL DEFAULT '',
    amount        numeric(14, 2) NOT NULL,
    direction     text NOT NULL,
    balance_after numeric(14, 2),
    category      text NOT NULL DEFAULT '',
    essential     boolean NOT NULL DEFAULT false,
    recurring     boolean NOT NULL DEFAULT false
);

-- Every visualization query filters by analysis and windows by date.
CREATE INDEX transactions_analysis_id_txn_date_idx ON transactions (analysis_id, txn_date);

-- +goose Down
DROP TABLE transactions;
DROP TABLE analyses;
