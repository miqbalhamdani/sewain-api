DB_NAME ?= sewain_dev
DOCS    ?= ../docs

# Load .env when one exists. The defaults compiled into cmd/api already target
# a host install, so a .env is only needed to deviate from them.
LOAD_ENV = set -a; [ -f .env ] && . ./.env; set +a;

.PHONY: dev worker scheduler db-create storage-init migrate migrate-down generate generated-diff fmt-check vet lint test test-iso test-race lint-imports lint-rls check

## dev: run the API against host PostgreSQL and Redis
dev:
	@$(LOAD_ENV) go run ./cmd/api

## worker: consume the job stream -- proof reading today (S1-040)
worker:
	@$(LOAD_ENV) go run ./cmd/worker

## scheduler: enqueue scheduled work; only the lease holder fires (S1-040)
scheduler:
	@$(LOAD_ENV) go run ./cmd/scheduler

## db-create: create the local development database if it is not there yet
db-create:
	@psql -lqtA -F'|' | cut -d'|' -f1 | grep -qx '$(DB_NAME)' || createdb '$(DB_NAME)'
	@echo "$(DB_NAME) ready"

## storage-init: create the private bucket and the pending/ 24h expiry (S1-033)
storage-init:
	@$(LOAD_ENV) go run ./cmd/storage-init

## migrate: apply every migration and exit
migrate:
	@$(LOAD_ENV) go run ./cmd/migrate

## migrate-down: roll back N migrations. Local only -- production is forward-only.
migrate-down:
	@$(LOAD_ENV) go run ./cmd/migrate -down $(or $(N),1)

GENERATED = internal/http/gen.go internal/db/sqlcgen

## generate: $(DOCS)/openapi.yaml -> internal/http, db/ -> internal/db/sqlcgen
generate:
	@test -f $(DOCS)/openapi.yaml \
		|| { echo "$(DOCS)/openapi.yaml not found -- the contract lives in the sibling docs repo"; exit 1; }
	@go tool oapi-codegen -config oapi-codegen.yaml $(DOCS)/openapi.yaml
	@sqlc generate

## generated-diff: fail if the tree does not match what the generators produce
generated-diff:
	@git diff --exit-code -- $(GENERATED) \
		|| { echo "generated code is stale -- commit the result of 'make generate'"; exit 1; }

fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	@go vet ./...

lint:
	@golangci-lint run

## test: needs PostgreSQL and Redis up -- see the note in cmd/api/health_test.go
test:
	@$(LOAD_ENV) go test ./...

## test-iso: owner isolation over every registered route, plus the harness's own checks
test-iso:
	@$(LOAD_ENV) go test ./internal/http/ -run 'TestOwnerIsolation|TestIsolationHarness' -v

## test-race: concurrency, including S1-023 -- the BR-022 double-booking race (internal/booking/race_test.go).
test-race:
	@$(LOAD_ENV) go test -race ./...

## lint-imports: only internal/db may own a connection (CLAUDE.md import rules).
##
## The check is on pgxpool, not pgx. pgx.Tx appears in InOwnerTx callback
## signatures on purpose, so every package that writes anything mentions it --
## linting that would either fail forever or have to exempt everyone. pgxpool is
## what actually opens connections, and confining it is the rule worth keeping.
##
## A four-line check instead of go-arch-lint: one rule does not need a tool.
lint-imports:
	@bad=$$(go list -deps -f '{{.ImportPath}} {{join .Imports " "}}' ./... \
		| awk '/jackc\/pgx\/v5\/pgxpool/ \
		       && $$1 ~ /^github.com\/miqbalhamdani\/sewain-api/ \
		       && $$1 != "github.com/miqbalhamdani/sewain-api/internal/db" { print "  " $$1 }'); \
	if [ -n "$$bad" ]; then echo "only internal/db may open a connection:"; echo "$$bad"; exit 1; fi

## lint-rls: fail if a owner table is not protected by row level security
lint-rls:
	@$(LOAD_ENV) go run ./cmd/lint-rls

## check: the bar for a PR. test covers test-iso -- this is the focused run.
check: generate generated-diff fmt-check vet lint lint-imports lint-rls test test-race
