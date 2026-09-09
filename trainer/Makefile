.DEFAULT_GOAL := help
SHELL := /bin/bash

VENV := .venv
PY := $(VENV)/bin/python
TRACE_DIR ?= ../traces
MODEL_OUT ?= model.txt
MODEL_BOUNDARY ?= 10m
REGISTRY_ADDR ?= localhost:8082

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[1m%-16s\033[0m %s\n", $$1, $$2}'

$(PY):
	python3 -m venv $(VENV)
	$(PY) -m pip install -q --upgrade pip
	$(PY) -m pip install -q -e '.[dev]'

.PHONY: venv
venv: $(PY) ## Create the virtualenv and install the package

.PHONY: proto
proto: $(PY) ## Regenerate the Python protobuf stubs
	# --python_out=. rather than into the package, because the generated modules import
	# each other as `belady.v1.*` and that path has to resolve from the same root.
	$(PY) -m grpc_tools.protoc \
		--proto_path=../api \
		--python_out=. --grpc_python_out=. \
		../api/belady/v1/*.proto
	@touch belady/__init__.py belady/v1/__init__.py

.PHONY: proto-check
proto-check: proto ## Fail if the generated stubs are stale
	@git diff --exit-code -- belady || { echo "generated Python stubs are stale: run make -C trainer proto"; exit 1; }

.PHONY: test
test: $(PY) ## Unit tests
	$(PY) -m pytest tests -q

.PHONY: lint
lint: $(PY) ## Static analysis
	$(VENV)/bin/ruff check .
	$(VENV)/bin/ruff format --check .

.PHONY: fmt
fmt: $(PY) ## Format
	$(VENV)/bin/ruff format .
	$(VENV)/bin/ruff check --fix .

.PHONY: audit
audit: $(PY) ## Check dependencies for known vulnerabilities
	$(VENV)/bin/pip-audit

.PHONY: boundary
boundary: $(PY) ## Report the reuse times in a captured trace
	$(PY) -m belady_trainer boundary --traces $(TRACE_DIR)

.PHONY: train
train: $(PY) ## Fit a model from a captured trace and publish it to the registry
	MODEL_BOUNDARY=$(MODEL_BOUNDARY) $(PY) -m belady_trainer train \
		--traces $(TRACE_DIR) --out $(MODEL_OUT) --publish $(REGISTRY_ADDR)

.PHONY: train-only
train-only: $(PY) ## Fit a model without publishing
	MODEL_BOUNDARY=$(MODEL_BOUNDARY) $(PY) -m belady_trainer train \
		--traces $(TRACE_DIR) --out $(MODEL_OUT) --publish ''

.PHONY: clean
clean:
	rm -rf $(MODEL_OUT) .pytest_cache .ruff_cache **/__pycache__
