.DEFAULT_GOAL := help
SHELL := /bin/bash

BIN_DIR ?= bin
BINARY  ?= watchtower
PKG     ?= ./cmd/app

.PHONY: help
help: ## list targets
	@awk 'BEGIN{FS=":.*##"; printf "Usage:\n  make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-18s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## build the watchtower binary
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags="-s -w" -o $(BIN_DIR)/$(BINARY) $(PKG)

.PHONY: run
run: ## run with .env applied
	@set -a && [ -f .env ] && source .env; set +a; go run $(PKG)

.PHONY: test
test: ## run unit tests
	go test ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy
	go mod tidy

# --- PostgreSQL / sqlc / migrations ------------------------------------------

# Override on the command line if your local DSN differs, e.g.
#   make migrate POSTGRES_DSN=postgres://u:p@host:5432/db?sslmode=disable
POSTGRES_DSN ?= postgres://watchtower:watchtower@localhost:5433/watchtower?sslmode=disable

.PHONY: pg-up
pg-up: ## start the local Postgres service (compose)
	docker compose -f deploy/docker-compose.yml up -d postgres

.PHONY: pg-down
pg-down: ## stop the local Postgres service
	docker compose -f deploy/docker-compose.yml stop postgres

.PHONY: migrate
migrate: ## apply DB migrations
	go run ./cmd/cli migrate -dsn "$(POSTGRES_DSN)"

.PHONY: sqlc
sqlc: ## regenerate db/sqlc Go code (requires sqlc binary on PATH)
	sqlc generate

.PHONY: pg-test
pg-test: ## run repository integration tests against the local Postgres
	POSTGRES_TEST_DSN="$(POSTGRES_DSN)" go test ./internal/infra/repository/... -count=1

.PHONY: docker-build
docker-build: ## build container image
	docker build -t polymarket-watchtower:dev .

.PHONY: up
up: ## start the local stack (app + prometheus + grafana)
	docker compose -f deploy/docker-compose.yml up -d --build

.PHONY: down
down: ## stop the local stack
	docker compose -f deploy/docker-compose.yml down

.PHONY: logs
logs: ## tail watchtower logs
	docker compose -f deploy/docker-compose.yml logs -f watchtower
