# Load .env if present and export its variables to every recipe.
-include .env
export

GO        ?= go
BIN       := bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X github.com/iamBlissMedik/irate-core-go/internal/app.Version=$(VERSION)
GOBIN     := $(shell $(GO) env GOPATH)/bin
SQLC      := $(GOBIN)/sqlc
LINT      := $(GOBIN)/golangci-lint

.DEFAULT_GOAL := help

## help: list targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | column -t -s ':'

## run: run the API locally (loads .env)
run:
	$(GO) run ./cmd/api

## build: compile api, migrate and seed into ./bin
build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/api ./cmd/api
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/migrate ./cmd/migrate
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN)/seed ./cmd/seed

## test: unit tests with the race detector (no database needed)
test:
	$(GO) test -race -count=1 ./...

## test-integration: full HTTP tests against TEST_DATABASE_URL / TEST_REDIS_URL (WIPES them)
test-integration:
	@test -n "$(TEST_DATABASE_URL)" || (echo "TEST_DATABASE_URL is not set" && exit 1)
	@test -n "$(TEST_REDIS_URL)" || (echo "TEST_REDIS_URL is not set" && exit 1)
	$(GO) test -tags integration -race -count=1 ./internal/integration/...

## cover: unit + integration coverage report (coverage.html)
cover:
	$(GO) test -tags integration -race -count=1 -coverpkg=./internal/... -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1
	$(GO) tool cover -html=coverage.out -o coverage.html

## lint: golangci-lint (config in .golangci.yml)
lint:
	$(LINT) run ./...

## fmt: format code (gofmt + goimports)
fmt:
	$(LINT) fmt ./...

## sqlc: regenerate internal/db from migrations + internal/db/queries
sqlc:
	$(SQLC) generate

## sqlc-check: fail if generated code is out of date (used in CI)
sqlc-check:
	$(SQLC) diff

## migrate-up: apply pending migrations
migrate-up:
	$(GO) run ./cmd/migrate up

## migrate-down: roll back the latest migration
migrate-down:
	$(GO) run ./cmd/migrate down

## migrate-status: show migration state
migrate-status:
	$(GO) run ./cmd/migrate status

## migrate-new: create a migration, e.g. make migrate-new name=add_payouts
migrate-new:
	@test -n "$(name)" || (echo "usage: make migrate-new name=<snake_case_name>" && exit 1)
	$(GO) run github.com/pressly/goose/v3/cmd/goose@v3.29.0 -dir migrations -s create $(name) sql

## seed: reset the database and load demo data (refuses in production)
seed:
	$(GO) run ./cmd/seed

## docker-up: start Postgres, Redis, migrations and the API in Docker
docker-up:
	docker compose up --build -d
	@echo "API: http://localhost:8000  (seed with: docker compose run --rm seed)"

## docker-down: stop the Docker stack (add v=1 to delete data)
docker-down:
	docker compose down $(if $(v),-v,)

## tools: install sqlc and golangci-lint
tools:
	$(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.6.2

.PHONY: help run build test test-integration cover lint fmt sqlc sqlc-check migrate-up migrate-down migrate-status migrate-new seed docker-up docker-down tools
