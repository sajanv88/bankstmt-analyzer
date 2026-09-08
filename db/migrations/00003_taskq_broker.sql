-- +goose Up
-- Backing table for github.com/Nuvraxis/taskQ's Postgres broker, vendored
-- verbatim (minus comments) from pgbroker/schema.sql at taskQ v1.0.0.
-- taskQ ships no migration tooling of its own, so carrying its DDL here
-- keeps "--migrate" a single complete path to a working database. The
-- IF NOT EXISTS clauses are upstream's and are kept so this stays a no-op
-- on a database where pgbroker/schema.sql was already applied by hand.
--
-- Keep in sync when upgrading taskQ: diff pgbroker/schema.sql against this
-- file and add a new migration for any change.
CREATE TABLE IF NOT EXISTS taskq_messages (
    sort_key       BIGINT GENERATED ALWAYS AS IDENTITY,
    id             TEXT NOT NULL,
    queue          TEXT NOT NULL,
    payload        BYTEA NOT NULL,
    attempts       INT NOT NULL DEFAULT 0,
    max_retry      INT NOT NULL DEFAULT 0,
    enqueued_at    TIMESTAMPTZ NOT NULL,
    locked_until   TIMESTAMPTZ,
    receipt_handle TEXT,
    PRIMARY KEY (sort_key)
);

CREATE INDEX IF NOT EXISTS taskq_messages_dequeue_idx
    ON taskq_messages (queue, locked_until, sort_key);

-- +goose Down
DROP TABLE IF EXISTS taskq_messages;
