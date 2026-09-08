-- +goose Up
-- Uploads and their constituent PDF files. Both ids are uuids assigned by
-- the application (google/uuid) rather than the database, so no pgcrypto
-- extension is required and a handler can reference an id before commit.
CREATE TABLE uploads (
    id             uuid PRIMARY KEY,
    status         text NOT NULL CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    failure_reason text,
    file_count     integer NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- Serves the operational "what is stuck in processing / failed recently"
-- scan; the column order matches that predicate (equality then range).
CREATE INDEX uploads_status_created_at_idx ON uploads (status, created_at);

CREATE TABLE upload_files (
    id           uuid PRIMARY KEY,
    upload_id    uuid NOT NULL REFERENCES uploads (id) ON DELETE CASCADE,
    -- position preserves the order the client submitted the files in, so
    -- the "--- STATEMENT n ---" blocks handed to the LLM are stable across
    -- re-runs of the same upload. Sorting by filename or insert time would
    -- not be.
    position     integer NOT NULL,
    filename     text NOT NULL,
    content_type text NOT NULL,
    size_bytes   bigint NOT NULL,
    storage_key  text NOT NULL,
    page_count   integer,
    ocr_markdown text,
    UNIQUE (upload_id, position)
);

CREATE INDEX upload_files_upload_id_idx ON upload_files (upload_id);

-- +goose Down
DROP TABLE upload_files;
DROP TABLE uploads;
