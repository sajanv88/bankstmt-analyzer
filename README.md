# bankstmt-analyzer

Ingests bank statement PDFs, runs them through an Azure-hosted Mistral OCR
endpoint and an Azure OpenAI GPT deployment, and exposes the resulting
analysis and chart data over HTTP.

One binary serves both roles: the HTTP API and the background worker that
drives the analysis pipeline.

## Status

Scaffolding is in place: configuration, database schema and migrations,
blob storage, the HTTP server with health probes, and the process
lifecycle. The upload/status/visualization endpoints, the OCR and LLM
clients, and the taskQ pipeline follow.

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

Errors are returned as RFC 7807 problem documents:

```sh
curl -s localhost:8080/nope
# {"type":"about:blank","title":"Not Found","status":404,
#  "detail":"The requested resource does not exist.","instance":"/nope",
#  "request_id":"..."}
```

Tear the database down again with `make compose-down` (this deletes its
volume).

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
internal/db        pgx pool and the sqlc-generated query layer
internal/http      chi router, middleware, handlers, RFC 7807 responses
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
