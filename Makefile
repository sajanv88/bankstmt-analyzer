# bankstmt-analyzer
#
# Local values come from .env when it exists (copy .env.example), so the
# same targets work with or without a shell that has the variables exported.
-include .env
export

BINARY       ?= bankstmt-analyzer
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
DOCKER_IMAGE ?= ghcr.io/sajanv88/bankstmt-analyzer
MIGRATIONS   := db/migrations
CHART        := deploy/helm/bankstmt-analyzer
LDFLAGS      := -s -w -X main.version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help
help: ## List the available targets
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*?##/ {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

## --- build and run --------------------------------------------------------

.PHONY: build
build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/api

.PHONY: run
run: ## Run the API and worker in one process, migrating first
	go run ./cmd/api --migrate

# run-api and run-worker exist so each role can be run on its own with the
# environment still coming from .env. Running `go run ./cmd/api --worker`
# by hand does not work: only make loads .env, so the process would start
# with none of the required variables set.
#
# Both migrate. Migrations are idempotent and take a Postgres advisory
# lock, so whichever role is started first brings the schema up and the
# other finds nothing to do.

.PHONY: run-api
run-api: ## Run only the HTTP API locally
	go run ./cmd/api --api --migrate

.PHONY: run-worker
run-worker: ## Run only the background worker locally
	go run ./cmd/api --worker --migrate

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist

## --- quality --------------------------------------------------------------

.PHONY: test
test: ## Run the unit tests
	go test ./...

.PHONY: test-race
test-race: ## Run the tests under the race detector
	go test -race ./...

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format the source tree
	gofmt -w $(shell git ls-files '*.go' 2>/dev/null || echo .)

.PHONY: tidy
tidy: ## Tidy go.mod and go.sum
	go mod tidy

## --- code generation ------------------------------------------------------

.PHONY: sqlc
sqlc: ## Regenerate the sqlc query layer
	sqlc generate

.PHONY: swagger
swagger: ## Regenerate the Swagger definitions into docs/
	swag init --generalInfo cmd/api/main.go --output docs --parseInternal --parseDependency

## --- database -------------------------------------------------------------

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" up

.PHONY: migrate-down
migrate-down: ## Roll back the most recent migration
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" down

.PHONY: migrate-status
migrate-status: ## Show which migrations have been applied
	goose -dir $(MIGRATIONS) postgres "$(DATABASE_URL)" status

## --- containers -----------------------------------------------------------

.PHONY: docker
docker: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(DOCKER_IMAGE):$(VERSION) .

.PHONY: compose-up
compose-up: ## Start the local Postgres
	docker compose up -d --wait

.PHONY: compose-down
compose-down: ## Stop the local Postgres and delete its volume
	docker compose down -v

## --- helm -----------------------------------------------------------------

.PHONY: helm-lint
helm-lint: ## Lint and render the chart against every ci/ values file
	@for values in $(CHART)/ci/*.yaml; do 		echo "== $$values"; 		helm lint $(CHART) --values "$$values" || exit 1; 		helm template lint-check $(CHART) --values "$$values" > /dev/null || exit 1; 	done

.PHONY: helm-package
helm-package: ## Package the chart into dist/
	helm package $(CHART) --version $(patsubst v%,%,$(VERSION)) --app-version $(VERSION) --destination dist
