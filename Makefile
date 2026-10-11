MODULE  := github.com/alex/mediad

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.DEFAULT_GOAL := help
.PHONY: help up down clean logs ui psql psql-temporal worker api all migrate config build test test-db tidy fmt vet workflowcheck replay capture inspect-histories check check-ci

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

## --- infrastructure -------------------------------------------------------

up: ## Start Postgres, Temporal and the Web UI
	@test -f .env || { echo "no .env — copy .env.example and fill it in"; exit 1; }
	docker compose up -d --wait postgresql temporal temporal-ui
	@echo "Temporal Web UI: http://localhost:8233"

down: ## Stop the stack (keeps all data)
	docker compose down

clean: ## Stop the stack and delete Postgres data and Temporal state
	docker compose down -v
	rm -rf ./data/postgres

logs: ## Tail the Temporal server log
	docker compose logs -f temporal

ui: ## Open the Temporal Web UI
	open http://localhost:8233

psql: ## Open psql on the mediad database
	psql "$$(grep -E '^MEDIAD_POSTGRES_DSN=' .env 2>/dev/null | cut -d= -f2- | tr -d '\"')"

psql-temporal: ## Open psql on the Temporal database
	docker compose exec postgresql psql -U $${POSTGRES_USER:-temporal} -d temporal

## --- application ----------------------------------------------------------

worker: ## Run the Temporal worker in this terminal
	go run -ldflags '$(LDFLAGS)' ./cmd/mediad worker

api: ## Run the HTTP API and web UI in this terminal (:8080)
	go run -ldflags '$(LDFLAGS)' ./cmd/mediad api

all: ## Run the worker and the API in one process
	go run -ldflags '$(LDFLAGS)' ./cmd/mediad all

migrate: ## Apply database migrations and exit
	go run ./cmd/mediad migrate

config: ## Print the effective configuration, secrets redacted
	@go run ./cmd/mediad config

## --- development ----------------------------------------------------------

build: ## Compile the binary into ./bin
	@mkdir -p bin
	go build -ldflags '$(LDFLAGS)' -o bin/mediad ./cmd/mediad

test: ## Run the tests (database tests are skipped without a server)
	go test ./... -count=1

test-db: ## Run the tests including those that need Postgres
	@test -f .env || { echo "no .env — copy .env.example and fill it in"; exit 1; }
	MEDIAD_TEST_DSN="$$(grep -E '^MEDIAD_POSTGRES_DSN=' .env | cut -d= -f2- | tr -d '\"')" \
		go test ./... -count=1

tidy: ## Sync go.mod / go.sum
	go mod tidy

fmt: ## Format the tree
	gofmt -l -w ./cmd ./internal

vet: ## Run go vet
	go vet ./...

WORKFLOWCHECK := $(shell go env GOPATH)/bin/workflowcheck

workflowcheck: ## Static check for non-deterministic calls in workflow code
	@test -x "$(WORKFLOWCHECK)" || go install go.temporal.io/sdk/contrib/tools/workflowcheck@latest
	@# Run through go vet rather than invoking workflowcheck directly, and that
	@# is not cosmetic: workflowcheck exits 0 even when it reports a problem, so
	@# a CI job calling it directly would pass on a broken workflow. go vet
	@# turns the diagnostics into a non-zero exit.
	go vet -vettool "$(WORKFLOWCHECK)" ./...

replay: ## Replay every recorded history against the current workflow code
	go test ./internal/request/ -run TestReplay -count=1 -v

capture: ## Record histories into the replay corpus (pass ids: make capture IDS="a b")
	@test -n "$(IDS)" || { echo 'usage: make capture IDS="download-watcher movie-603"'; exit 2; }
	./scripts/capture-histories.sh $(IDS)

inspect-histories: ## Decode the corpus so it can be reviewed before committing
	./scripts/inspect-history.sh 'internal/request/testdata/histories/*.json' 

check: fmt vet build test ## Format, vet, build and test
	@echo "all checks passed"

check-ci: workflowcheck check test-db ## What CI runs: determinism, build, tests
	@MEDIAD_REQUIRE_REPLAY=1 go test ./internal/request/ -run TestReplay -count=1
	@echo "all CI checks passed"
