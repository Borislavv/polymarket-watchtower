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
