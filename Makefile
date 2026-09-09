.DEFAULT_GOAL := help
SHELL := /bin/bash
GO ?= go
COMPOSE ?= docker compose -f deploy/compose.yaml
BIN := bin
SERVICES := gateway cachenode registry origin loadgen

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build every service into ./bin
	@mkdir -p $(BIN)
	@for s in $(SERVICES); do echo "  build $$s"; $(GO) build -trimpath -o $(BIN)/$$s ./cmd/$$s || exit 1; done

.PHONY: test
test: ## Unit tests with the race detector
	$(GO) test ./... -race -count=1

.PHONY: cover
cover: ## Unit tests with a coverage report
	$(GO) test ./... -covermode=atomic -coverprofile=coverage.txt
	$(GO) tool cover -func=coverage.txt | tail -1

.PHONY: bench-micro
bench-micro: ## Go microbenchmarks (model eval, store lookup, ring buffer)
	$(GO) test ./internal/... -run '^$$' -bench . -benchmem

.PHONY: lint
lint: ## Static analysis
	$(GO) vet ./...
	golangci-lint run

.PHONY: fmt
fmt: ## Format
	$(GO) fmt ./...
	golangci-lint fmt

.PHONY: proto
proto: ## Regenerate gRPC stubs from api/**/*.proto
	@command -v protoc-gen-go >/dev/null || $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@latest
	@command -v protoc-gen-go-grpc >/dev/null || $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
	PATH="$$($(GO) env GOPATH)/bin:$$PATH" protoc \
		--proto_path=api \
		--go_out=gen --go_opt=paths=source_relative \
		--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
		api/belady/v1/*.proto

.PHONY: proto-check
proto-check: proto ## Fail if generated stubs are stale
	@git diff --exit-code -- gen || { echo "generated protobuf code is stale: run make proto"; exit 1; }

.PHONY: up
up: ## Bring up the cluster
	$(COMPOSE) up -d --build
	@echo "gateway  localhost:8080 (grpc)"
	@echo "grafana  http://localhost:3000"

.PHONY: down
down: ## Tear down the cluster and its volumes
	$(COMPOSE) down -v

.PHONY: logs
logs: ## Follow cluster logs
	$(COMPOSE) logs -f

.PHONY: bench
bench: ## Replay a workload against the running cluster and report hit ratio
	$(GO) run ./cmd/loadgen

.PHONY: train
train: ## Train a model from a captured trace and publish it
	$(MAKE) -C trainer train

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.txt
