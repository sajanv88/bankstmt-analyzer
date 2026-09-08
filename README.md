# bankstmt-analyzer

Ingests bank statement PDFs, runs them through an Azure-hosted Mistral OCR
endpoint and an Azure OpenAI GPT deployment, and exposes the resulting
analysis and chart data over HTTP.

One binary serves both roles: the HTTP API and the background worker that
drives the analysis pipeline.

## Status

Functionally complete: uploads are OCR'd, analysed and served. What remains
is packaging — the Dockerfile, the Helm chart and the CI workflows.

`prompts/analysis_system.md` still holds a placeholder prompt. Replace it
with the real one before pointing this at a live deployment; it is embedded
at build time, so changing it means rebuilding.

## Requirements

- Go 1.26+
- Docker (for the local Postgres)
- `sqlc`, `goose`, `swag` and `golangci-lint` for the generation and
  quality targets

## Configuration

Everything is read from the environment; nothing is read from a config
file. Copy `.env.example` to `.env` and fill it in.

### Required

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | pgx connection string. Backs both the application tables and the taskQ queue. |
| `STORAGE_DIR` | Local directory that holds uploaded PDFs. |
| `AZURE_OCR_ENDPOINT` | Azure Mistral OCR endpoint. |
| `AZURE_OCR_API_KEY` | Azure Mistral OCR key. |
| `AZURE_OCR_MODEL` | OCR model name, e.g. `mistral-ocr-2503`. |
| `AZURE_OPENAI_ENDPOINT` | Azure OpenAI endpoint. |
| `AZURE_OPENAI_API_KEY` | Azure OpenAI key. |
| `AZURE_OPENAI_DEPLOYMENT` | Azure OpenAI deployment name. |

### Optional

| Variable | Default | Purpose |
| --- | --- | --- |
| `ENV` | `development` | Set to `production` to switch off Swagger. |
| `HTTP_ADDR` | `:8080` | API listen address. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `MIGRATE_ON_START` | `false` | Apply migrations during startup; same effect as `--migrate`. |
| `POSTGRES_PORT` | `5432` | Host port `docker-compose` publishes Postgres on. |
| `AZURE_OCR_PATH` | `/providers/mistral/azure/ocr` | Route appended to the OCR endpoint. |
| `WORKER_CONCURRENCY` | `4` | Saga hops processed in parallel. |
| `WORKER_MAX_RETRY` | `3` | Redeliveries per saga step, so 4 attempts each. |
| `TASKQ_LEASE_DURATION` | `15m` | Must exceed the slowest single step, or an in-flight message is redelivered while still being worked on. |

`.env.example` lists the remaining tunables (Azure timeouts, HTTP
timeouts, CORS origins, worker concurrency and retry budget, queue lease
duration, upload limits) alongside their defaults.

## Running locally

```sh
cp .env.example .env      # then fill in the Azure values
make compose-up           # postgres:16 on POSTGRES_PORT
make run                  # migrates, then serves on HTTP_ADDR
```

Check it is alive:

```sh
curl -s localhost:8080/healthz    # {"status":"ok"}
curl -s localhost:8080/readyz     # {"status":"ok"} once the database answers
```

Tear the database down again with `make compose-down` (this deletes its
volume).

## API

Base path `/api/v1`. Every error is an RFC 7807 problem document served as
`application/problem+json`.

### Upload statements

Between 1 and 12 PDFs, each at most 20 MB, in the repeated `files` field.
Files are validated by their `%PDF-` magic bytes, not by the content type
the client claims.

```sh
curl -s -X POST localhost:8080/api/v1/uploads   -F "files=@january.pdf"   -F "files=@february.pdf"

# 202 Accepted
# {"id":"c6148f86-1e95-443a-836b-e7e949872e44","status":"pending"}
```

### Poll the status

```sh
curl -s localhost:8080/api/v1/uploads/c6148f86-.../status

# {"id":"c6148f86-...","status":"completed",
#  "created_at":"2025-03-01T10:00:00Z","updated_at":"2025-03-01T10:04:12Z",
#  "analysis_id":"11111111-..."}
```

`failure_reason` appears only when the status is `failed`, and
`analysis_id` only when it is `completed`.

### Fetch the visualization

Valid only once the upload is `completed`; otherwise it answers 409. The
default window is the last 3 months that carry data. `months` (1-12)
counts back from the last month with data, and `from`/`to` (`YYYY-MM`)
override it — supply just one to anchor that end.

```sh
# default window
curl -s "localhost:8080/api/v1/uploads/c6148f86-.../visualization"

# a single month
curl -s "localhost:8080/api/v1/uploads/c6148f86-.../visualization?months=1"

# an explicit range
curl -s "localhost:8080/api/v1/uploads/c6148f86-.../visualization?from=2025-01&to=2025-02"
```

```json
{
  "currency": "EUR",
  "period": { "from": "2025-01", "to": "2025-03" },
  "present": {
    "monthly_income_vs_spending": [
      { "month": "2025-01", "income": 3000, "spending": 250, "net": 2750 }
    ],
    "category_breakdown": [
      { "category": "Rent", "amount": 800, "transaction_count": 1, "share": 0.59 }
    ],
    "monthly_by_category": [
      { "month": "2025-01", "category": "Groceries", "amount": 200 }
    ],
    "essential_vs_discretionary": [
      { "month": "2025-01", "essential": 200, "discretionary": 50 }
    ],
    "balance_over_time": [{ "date": "2025-01-10", "balance": 2750 }]
  },
  "forecast": { "projected_savings": [{ "month": "2025-04", "amount": 812.5 }] },
  "summary": {
    "total_income": 6000, "total_spending": 1350, "net_savings": 4650,
    "savings_rate": 0.775, "essential_spending": 1300,
    "discretionary_spending": 50, "recurring_spending": 800,
    "average_monthly_income": 2000, "average_monthly_spending": 450,
    "transaction_count": 6, "month_count": 3
  }
}
```

`present` is always recomputed from the `transactions` table for the window
asked for, so it matches the requested range rather than whatever window
the model happened to chart. `forecast` is a projection and cannot be
recomputed, so it is served as stored.

### Errors

```sh
curl -s localhost:8080/api/v1/uploads/nope/status
# {"type":"about:blank","title":"Bad Request","status":400,
#  "detail":"The upload id must be a uuid.","instance":"/api/v1/uploads/nope/status",
#  "request_id":"...","invalid_params":[{"name":"id","reason":"not a valid uuid"}]}
```

## The analysis pipeline

The worker runs a [taskQ](https://github.com/Nuvraxis/taskQ) saga over the
Postgres broker. Each step is one hop through the queue, so a worker
restart resumes where the run left off rather than starting over.

| Step | Does | Compensation |
| --- | --- | --- |
| `mark_processing` | Moves the upload out of `pending` and clears any stale failure reason. | — |
| `ocr` | Calls Azure OCR for each file and stores its markdown and page count. Files that already have output are skipped. | — |
| `analyze` | Builds the `--- STATEMENT n ---` message, calls Azure OpenAI at temperature 0 with a JSON response format, validates the reply, and writes the analysis and its transactions in one database transaction. | Deletes the analysis; transactions cascade. |
| `mark_completed` | Marks the upload `completed`. | — |

Any failure marks the upload `failed` with `failure_reason` set to
`"<step>: <reason>"`, after compensation has unwound whatever succeeded.
Configured secrets are stripped from that message before it is logged,
carried through the queue, or stored.

The whole workflow is idempotent per upload id, so re-running it is safe:
OCR is skipped for files that already have output, and the analysis is
replaced rather than appended.

Two things worth knowing about the retry model. taskQ's retry budget is
per hop (`WORKER_MAX_RETRY`, four attempts by default) and it applies to
every failure, so a permanently unusable model reply is retried before the
run gives up — the clients classify errors as retryable or not, and that
classification is logged, but taskQ has no way to act on it. And
`TASKQ_LEASE_DURATION` must stay comfortably above the slowest step: a
lease that expires mid-OCR gets the message redelivered to a second
worker while the first is still working.

## Swagger

Served at <http://localhost:8080/swagger/index.html> whenever `ENV` is not
`production`; in production the route is absent and returns 404. Regenerate
the document with `make swagger` after changing any handler annotation or
DTO — CI fails if it is stale.

## Run modes

| Invocation | Behaviour |
| --- | --- |
| `bankstmt-analyzer` | API and worker in one process. |
| `bankstmt-analyzer --api` | API only. |
| `bankstmt-analyzer --worker` | Worker only. |
| `bankstmt-analyzer --migrate` | Migrate, then run the selected roles. |
| `bankstmt-analyzer --migrate --api=false --worker=false` | Migrate and exit. Used by the Helm pre-upgrade Job. |

Migrations take a Postgres advisory lock, so replicas starting at once
serialise rather than collide.

## Make targets

`make help` lists them all. The common ones:

| Target | Purpose |
| --- | --- |
| `make build` | Build into `bin/`. |
| `make run` | Migrate and run locally. |
| `make test` / `make test-race` | Run the tests. |
| `make lint` | Run `golangci-lint`. |
| `make sqlc` | Regenerate the query layer from `internal/db/queries`. |
| `make swagger` | Regenerate the Swagger definitions into `docs/`. |
| `make migrate-up` / `make migrate-down` / `make migrate-status` | Drive goose directly. |
| `make compose-up` / `make compose-down` | Local Postgres. |
| `make docker` | Build the container image. |

## Layout

```
cmd/api            entrypoint: flags, wiring, lifecycle
internal/config    environment-driven configuration
internal/db        pgx pool, transaction helpers, sqlc-generated queries
internal/http      chi router, middleware, handlers, RFC 7807 responses
internal/ocr       Azure Mistral OCR client
internal/llm       Azure OpenAI client and the response contract
internal/pipeline  the taskQ saga, its steps and their compensations
internal/logging   slog JSON logger construction
internal/migrate   embedded goose migration runner
internal/storage   BlobStore interface and its local-disk implementation
db/migrations      goose SQL migrations, embedded into the binary
prompts            the analysis system prompt, embedded at build time
```

`db/migrations/00003_taskq_broker.sql` carries taskQ's own broker table.
taskQ ships no migration tooling, so vendoring its DDL keeps `--migrate` a
single complete path to a working database; re-diff it against
`pgbroker/schema.sql` when upgrading taskQ.
