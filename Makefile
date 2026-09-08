# Makefile - every command this repo needs, on Windows, Linux and macOS alike.
#
#   make            list the targets
#   make check      the fast gates worth running before every commit
#   make ci         everything CI runs, in CI's order
#
# WINDOWS: this needs Git Bash. If anything cannot find its tools, run `make doctor`.

# ---------------------------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------------------------
# .env holds the values the service itself reads (see .env.example). It is included so that the
# run and migrate targets work without exporting anything by hand.
-include .env

# Makefile.local is gitignored and optional: per-machine settings that should win over .env.
# It is included AFTER .env deliberately. A .env line is a plain recursive assignment, so the
# last one wins - reading Makefile.local first would let .env quietly override the very settings
# it exists to pin.
-include Makefile.local

export

# ---------------------------------------------------------------------------------------------
# Shell
# ---------------------------------------------------------------------------------------------
# Every recipe runs under bash so one set of recipes works everywhere: -e stops on the first
# failure, -u catches misspelled variables, and -o pipefail keeps a failure inside a pipeline
# from being masked by a successful tail.
#
# On Windows, finding bash is the hard part. A bare `bash` on PATH is Git Bash when make is run
# from a Git Bash prompt - but from PowerShell or cmd it is usually WSL's
# (C:\Windows\System32\bash.exe), which has its own filesystem and PATH and cannot see the
# Windows Go toolchain. The symptom is an unhelpful "go: command not found".
#
# So Git Bash is located directly rather than through PATH. The glob is how the space in
# "Program Files" is dodged: make cannot hold that path as a single word in a list, but
# $(wildcard) can produce it, and SHELL accepts the result verbatim.
#
# Override from the command line or Makefile.local if Git lives elsewhere:
#   make GIT_BASH='D:/tools/Git/bin/bash.exe' <target>
ifeq ($(OS),Windows_NT)
  EXE := .exe
  GIT_BASH ?= $(wildcard C:/Progra*/Git/bin/bash.exe)
  # Two matches (Git installed under both Program Files trees) are indistinguishable from one
  # path containing a space, so the value is unusable - fall back rather than guess wrong.
  ifneq ($(findstring exe C:,$(GIT_BASH)),)
    GIT_BASH :=
  endif
  ifeq ($(GIT_BASH),)
    SHELL := bash.exe
  else
    SHELL := $(GIT_BASH)
  endif
else
  SHELL := /bin/bash
  EXE   :=
endif
.SHELLFLAGS := -eu -o pipefail -c

# ---------------------------------------------------------------------------------------------
# Project
# ---------------------------------------------------------------------------------------------
BINARY       ?= bankstmt-analyzer
DOCKER_IMAGE ?= ghcr.io/sajanv88/bankstmt-analyzer
BIN_DIR      := bin
MIGRATIONS   := db/migrations
CHART        := deploy/helm/bankstmt-analyzer

# git describe fails outside a repository and on one with no tags; either way "dev" is the right
# answer rather than an aborted build.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

# Pinned to match .github/workflows/ci.yml. A generator that drifts from CI's produces a diff CI
# rejects, with an error that blames the committed code rather than the tool.
SQLC_VERSION          := v1.31.1
SWAG_VERSION          := v1.16.4
GOLANGCI_LINT_VERSION := v2.12.2

# ---------------------------------------------------------------------------------------------
# Test services
# ---------------------------------------------------------------------------------------------
# Set EXPLICITLY, never inherited. The integration tests run migrations against DATABASE_URL and
# write to the S3 bucket, and the `export` above puts .env's values in front of every recipe - so
# without pinning these, `make test` would target whatever database .env happens to name, which
# on someone's machine will eventually be a real one.
#
# `make test` therefore runs with them blank, which is exactly what makes those tests skip.
# `make test-integration` sets them from the values below, which default to the docker-compose
# services. Override per run, or in Makefile.local:
#   make test-integration TEST_DATABASE_URL='postgres://.../other?sslmode=disable'
POSTGRES_PORT ?= 5432
MINIO_PORT    ?= 9000

TEST_DATABASE_URL         ?= postgres://bankstmt:bankstmt@localhost:$(POSTGRES_PORT)/bankstmt?sslmode=disable
TEST_S3_ENDPOINT          ?= http://localhost:$(MINIO_PORT)
TEST_S3_BUCKET            ?= bankstmt-uploads
TEST_S3_ACCESS_KEY_ID     ?= bankstmt
TEST_S3_SECRET_ACCESS_KEY ?= bankstmt123

INTEGRATION_ENV := DATABASE_URL='$(TEST_DATABASE_URL)' S3_ENDPOINT='$(TEST_S3_ENDPOINT)' S3_BUCKET='$(TEST_S3_BUCKET)' S3_ACCESS_KEY_ID='$(TEST_S3_ACCESS_KEY_ID)' S3_SECRET_ACCESS_KEY='$(TEST_S3_SECRET_ACCESS_KEY)'
UNIT_ENV        := DATABASE_URL= S3_ENDPOINT= S3_BUCKET=

# Extra flags for the test targets, e.g. make test TESTFLAGS='-count=1'
TESTFLAGS ?=

.DEFAULT_GOAL := help

# ---------------------------------------------------------------------------------------------
# Verification - mirrors .github/workflows/ci.yml
# ---------------------------------------------------------------------------------------------

.PHONY: ci
ci: fmt-check vet tidy-check lint sqlc-check swagger-check test-integration helm-lint docker ## Everything CI runs, in CI's order

.PHONY: check
check: fmt-check vet lint sqlc-check swagger-check ## The fast gates worth running before a commit

.PHONY: build
build: ## Build the binary into bin/
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)$(EXE) ./cmd/api
	@echo "built -> $(BIN_DIR)/$(BINARY)$(EXE)  ($(VERSION))"

.PHONY: vet
vet: ## go vet ./...
	go vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

# .gitattributes pins the working tree to LF on every platform, so a plain gofmt check asks the
# same question here that CI asks on Linux. Without it, a Windows checkout would be CRLF and
# gofmt would report every file as unformatted.
.PHONY: fmt
fmt: ## Format the Go sources
	@offenders="$$(gofmt -l . || true)"; \
	if [ -z "$$offenders" ]; then \
	  echo "gofmt: nothing to do"; \
	else \
	  echo "gofmt -w:"; echo "$$offenders" | sed 's/^/  /'; \
	  gofmt -w $$offenders; \
	fi

.PHONY: fmt-check
fmt-check: ## Fail if any Go source is unformatted
	@offenders="$$(gofmt -l . || true)"; \
	if [ -n "$$offenders" ]; then \
	  echo "These files are not gofmt-formatted:"; echo "$$offenders" | sed 's/^/  /'; \
	  echo "Run 'make fmt'."; \
	  exit 1; \
	fi; \
	echo "gofmt: clean"

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

.PHONY: tidy-check
tidy-check: ## Fail if go.mod or go.sum is not tidy
	@go mod tidy
	@if ! git diff --exit-code -- go.mod go.sum; then \
	  echo "go.mod/go.sum were not tidy. 'make tidy' has just fixed them; commit the result."; \
	  exit 1; \
	fi; \
	echo "go mod: tidy"

# ---------------------------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------------------------

.PHONY: test
test: ## Run the unit tests (integration tests skip; touches no database or bucket)
	@$(UNIT_ENV) go test $(TESTFLAGS) ./...

.PHONY: test-race
test-race: ## Run the unit tests under the race detector
	@$(UNIT_ENV) go test -race $(TESTFLAGS) ./...

.PHONY: test-integration
test-integration: ## Run everything, including the Postgres and MinIO integration tests
	@echo "database: $(TEST_DATABASE_URL)"
	@echo "bucket:   $(TEST_S3_ENDPOINT)/$(TEST_S3_BUCKET)"
	@$(INTEGRATION_ENV) go test -race $(TESTFLAGS) ./...

.PHONY: test-pkg
test-pkg: ## Test one package: make test-pkg PKG=./internal/storage/...
	@[ -n "$(PKG)" ] || { echo "usage: make test-pkg PKG=./internal/storage/..."; exit 2; }
	@$(UNIT_ENV) go test $(TESTFLAGS) $(PKG)

.PHONY: test-run
test-run: ## Test by name: make test-run RUN=TestUploadStatus [PKG=./internal/http]
	@[ -n "$(RUN)" ] || { echo "usage: make test-run RUN=TestName [PKG=./internal/http]"; exit 2; }
	@$(UNIT_ENV) go test $(TESTFLAGS) -run '$(RUN)' $(if $(PKG),$(PKG),./...)

.PHONY: cover
cover: ## Report total statement coverage, integration tests included
	@$(INTEGRATION_ENV) go test -coverprofile=coverage.out ./... > /dev/null
	@go tool cover -func=coverage.out | tail -1
	@rm -f coverage.out

# ---------------------------------------------------------------------------------------------
# Code generation
# ---------------------------------------------------------------------------------------------

.PHONY: sqlc
sqlc: ## Regenerate the sqlc query layer
	sqlc generate

.PHONY: sqlc-check
sqlc-check: ## Fail if internal/db is out of date with the queries and schema
	@sqlc diff && echo "sqlc: up to date"

.PHONY: swagger
swagger: ## Regenerate the Swagger definitions into docs/
	swag init --generalInfo cmd/api/main.go --output docs --parseInternal --parseDependency

.PHONY: swagger-check
swagger-check: swagger ## Fail if docs/ is out of date with the handler annotations
	@if ! git diff --exit-code -- docs/; then \
	  echo "docs/ was stale. 'make swagger' has just regenerated it; commit the result."; \
	  exit 1; \
	fi; \
	echo "swagger: up to date"

.PHONY: generate
generate: sqlc swagger ## Regenerate everything that is committed but generated

.PHONY: tools
tools: ## Install the pinned generators and linter into GOBIN
	go install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)
	go install github.com/swaggo/swag/cmd/swag@$(SWAG_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

# ---------------------------------------------------------------------------------------------
# Running the service
# ---------------------------------------------------------------------------------------------
# These read .env through the include above, which is why a bare `go run ./cmd/api` fails with
# "DATABASE_URL is not set": only make loads that file.

.PHONY: run
run: ## Run the API and worker in one process, migrating first
	go run ./cmd/api --migrate

.PHONY: run-api
run-api: ## Run only the HTTP API
	go run ./cmd/api --api --migrate

.PHONY: run-worker
run-worker: ## Run only the background worker
	go run ./cmd/api --worker --migrate

# ---------------------------------------------------------------------------------------------
# Database
# ---------------------------------------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" down

.PHONY: migrate-status
migrate-status: ## Show which migrations have been applied
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" status

# ---------------------------------------------------------------------------------------------
# Local stack
# ---------------------------------------------------------------------------------------------

.PHONY: compose-up
compose-up: ## Start Postgres and MinIO, and create the bucket
	docker compose up -d --wait

.PHONY: compose-down
compose-down: ## Stop the local stack AND delete its data volumes
	docker compose down -v

.PHONY: compose-ps
compose-ps: ## Show the local stack's containers
	docker compose ps

.PHONY: compose-logs
compose-logs: ## Follow the local stack's logs
	docker compose logs -f

# ---------------------------------------------------------------------------------------------
# Containers and chart
# ---------------------------------------------------------------------------------------------

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(VERSION) .

.PHONY: helm-lint
helm-lint: ## Lint and render the chart against every values overlay
	@for values in $(CHART)/ci/*.yaml $(CHART)/values-dev.yaml; do \
	  echo "== $$values"; \
	  helm lint $(CHART) --values "$$values" > /dev/null || exit 1; \
	  helm template lint-check $(CHART) --values "$$values" > /dev/null || exit 1; \
	done; \
	echo "helm: every overlay lints and renders"

.PHONY: helm-package
helm-package: ## Package the chart into dist/
	helm package $(CHART) --version $(patsubst v%,%,$(VERSION)) --app-version $(patsubst v%,%,$(VERSION)) --destination dist

# ---------------------------------------------------------------------------------------------
# Housekeeping
# ---------------------------------------------------------------------------------------------

.PHONY: clean
clean: ## Remove build output and the Go test cache
	@rm -rf $(BIN_DIR) dist coverage.out
	@go clean -testcache
	@echo "cleaned"

.PHONY: doctor
doctor: ## Check that the tools these targets need are present and usable
	@echo "shell:    $(SHELL)"
	@echo "git bash: $(if $(GIT_BASH),$(GIT_BASH),(not detected - falling back to PATH))"
	@echo "version:  $(VERSION)"
	@echo ""
	@required=0; \
	for tool in go git bash; do \
	  if command -v $$tool > /dev/null 2>&1; then \
	    printf '  %-16s %s\n' "$$tool" "$$(command -v $$tool)"; \
	  else \
	    printf '  %-16s MISSING (required)\n' "$$tool"; required=1; \
	  fi; \
	done; \
	for tool in docker sqlc swag golangci-lint goose helm; do \
	  if command -v $$tool > /dev/null 2>&1; then \
	    printf '  %-16s %s\n' "$$tool" "$$(command -v $$tool)"; \
	  else \
	    printf '  %-16s missing (only some targets need it)\n' "$$tool"; \
	  fi; \
	done; \
	echo ""; \
	if ! command -v go > /dev/null 2>&1; then \
	  echo "The shell running these recipes cannot see the Go toolchain."; \
	  echo "On Windows this Makefile locates Git Bash itself, so yours is probably installed"; \
	  echo "somewhere the glob C:/Progra*/Git/bin/bash.exe does not reach. Point it at yours:"; \
	  echo ""; \
	  echo "    make GIT_BASH='D:/path/to/Git/bin/bash.exe' <target>"; \
	  echo ""; \
	  echo "or set GIT_BASH once in Makefile.local. Without it make falls back to whatever"; \
	  echo "'bash' is on PATH, which from PowerShell or cmd is usually WSL's - a separate"; \
	  echo "filesystem with no Windows Go toolchain in it."; \
	  echo ""; \
	fi; \
	echo "  'make tools' installs sqlc, swag and golangci-lint at the pinned versions."; \
	echo ""; \
	echo "test database: $(TEST_DATABASE_URL)"; \
	echo "test bucket:   $(TEST_S3_ENDPOINT)/$(TEST_S3_BUCKET)"; \
	exit $$required

.PHONY: help
help: ## List the available targets
	@echo "$(BINARY) - make <target>"
	@echo ""
	@grep -hE '^[a-zA-Z_-]+:.*## .*$$' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*## "}; {printf "  %-18s %s\n", $$1, $$2}'
	@echo ""
	@echo "  Run 'make doctor' if a target cannot find its tools."
