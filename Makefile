# := set in stone
# = lazy one and doesnt compute until used
# ?= uses default value if there isnt one provided

.DEFAULT_GOAL := help # this is what command runs if we just run "make" without any parameters
SHELL := /bin/bash
GO ?= go
BIN := bin

# Compose's project directory is deploy/, so every relative path inside the compose
# files resolves from there. .env lives at the repo root instead, which is where you
# would look for it, so point Compose at it explicitly when it exists.
ENV_FILE := $(wildcard .env)
COMPOSE_FLAGS := $(if $(ENV_FILE),--env-file $(ENV_FILE),)
COMPOSE ?= docker compose -f deploy/compose.yaml $(COMPOSE_FLAGS)
COMPOSE_DEV ?= docker compose -f deploy/compose.yaml -f deploy/compose.dev.yaml $(COMPOSE_FLAGS)
SERVICES := gateway cachenode registry origin loadgen

# Pinned so a local run and CI report the same findings. A newer staticcheck finds
# things an older one does not, which shows up as a red build on a green working tree.
# The same version is pinned in .github/workflows/ci.yml; move both together.
GOLANGCI_VERSION := v2.13.2
GOLANGCI := $(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

# The generated stubs record the versions that wrote them, so these are what makes
# `make proto-check` a check on the protos rather than on whoever ran it last.
PROTOC_VERSION := 35.0
PROTOC_GEN_GO_VERSION := v1.36.12
PROTOC_GEN_GO_GRPC_VERSION := v1.6.2

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
	$(GOLANGCI) run

.PHONY: fmt
fmt: ## Format
	$(GO) fmt ./...
	$(GOLANGCI) fmt

.PHONY: proto
proto: ## Regenerate gRPC stubs from api/**/*.proto
	@$(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	@$(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	@have=$$(protoc --version | awk '{print $$2}'); \
	 [ "$$have" = "$(PROTOC_VERSION)" ] || \
	 echo "warning: protoc $$have, but the committed stubs were written by $(PROTOC_VERSION); proto-check will report drift that is only a version header"
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
	@$(MAKE) --no-print-directory wait
	@echo "gateway     localhost:8080 (grpc)"
	@echo "registry    localhost:8082 (grpc)"
	@echo "prometheus  http://localhost:9091"
	@echo "grafana     http://localhost:3000"

.PHONY: up-dev
up-dev: ## Bring up the cluster with host-visible traces, models and node ports
	@mkdir -p traces models
	$(COMPOSE_DEV) up -d --build
	@$(MAKE) --no-print-directory wait

.PHONY: wait
wait: ## Block until every service reports healthy
	@./scripts/wait-for-health.sh

.PHONY: down
down: ## Tear down the cluster and its volumes
	$(COMPOSE_DEV) down -v --remove-orphans

.PHONY: logs
logs: ## Follow cluster logs
	$(COMPOSE) logs -f

.PHONY: images
images: ## Build the container images without starting anything
	$(COMPOSE) --profile train build

.PHONY: bench
bench: ## Replay a workload against the running cluster and report hit ratio
	$(GO) run ./cmd/loadgen

.PHONY: train
train: ## Train a model from a captured trace and publish it
	$(MAKE) -C trainer train

.PHONY: train-compose
train-compose: ## Train inside the cluster, reading the traces volume
	$(COMPOSE) --profile train run --rm --build trainer train

.PHONY: clean
clean:
	rm -rf $(BIN) coverage.txt
