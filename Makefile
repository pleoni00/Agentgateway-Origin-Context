SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help
.NOTPARALLEL:
PYTHON ?= python3.12
VENV ?= .venv
PY := $(abspath $(VENV)/bin/python)
PROJECT ?= agentgateway
PYTEST_ARGS ?= -q -ra
SERVICES ?=
TAIL ?= 100
export COMPOSE_PROJECT_NAME := $(PROJECT)
export LOCAL_UID ?= $(shell id -u)
export LOCAL_GID ?= $(shell id -g)
COMPOSE := docker compose --project-name $(PROJECT) -f compose.yaml

.PHONY: help setup env up down build status logs follow test test-go test-integration check curl demo migrate reset
help: ## List available commands
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z0-9_-]+:.*## / {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)
setup: ## Install Python example and integration-test dependencies
	@test -x "$(PY)" || $(PYTHON) -m venv "$(VENV)"
	$(PY) -m pip install -r requirements.lock
env: ## Generate control-plane secrets once
	$(PYTHON) scripts/init_env.py
up: env ## Build, migrate and start the complete example
	$(COMPOSE) up -d --build --wait
	$(PY) scripts/wait_ready.py
down: env ## Stop containers and preserve persistent data
	$(COMPOSE) down
build: env ## Build images
	$(COMPOSE) build
migrate: env ## Apply versioned DB migrations and one-time example seed
	$(COMPOSE) run --rm migrate
status: env ## Show container status
	$(COMPOSE) ps -a
logs: env ## Show logs; filter with SERVICES="..."
	$(COMPOSE) logs --tail=$(TAIL) $(SERVICES)
follow: env ## Follow logs
	$(COMPOSE) logs -f --tail=$(TAIL) $(SERVICES)
test: test-go test-integration ## Run Go unit and live integration tests
test-go: ## Test Go identity, policy, persistence and lifecycle logic
	cd authorization && go test -race ./...
test-integration: ## Test the running example through its public gateway
	$(PY) -m pytest tests/ $(PYTEST_ARGS)
check: ## Validate Go, Python, shell and Compose configuration
	cd authorization && go vet ./...
	$(PY) -m compileall -q agents services mcp_servers llm_server scripts tests
	bash -n scripts/*.sh
	docker compose --env-file .env.example -f compose.yaml config --quiet
curl: ## Run authenticated example calls with redacted credentials
	PYTHON_BIN=$(PY) bash scripts/curl_flow.sh
demo: ## Run ERP -> A -> B -> C -> documents
	$(PY) -m services.erp_client finance_report '{}'
reset: down ## DESTRUCTIVE: delete authorization DB and Keycloak volume
	$(COMPOSE) down --volumes
	$(PYTHON) -c 'from pathlib import Path; [p.unlink(missing_ok=True) for p in Path("authorization/data").glob("permissions.db*")]'
