# Prerequisites:
#   - Go 1.25+
#   - sqlc (go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0)
#   - golang-migrate (brew install golang-migrate)
#   - CompileDaemon for `make dev` (go install github.com/githubnemo/CompileDaemon@latest)
#
# Database:
#   Targets that touch the database read DATABASE_URL from .env.development
#   (and .env.local if present). Start Postgres via `make db` or point
#   DATABASE_URL at any Postgres you already have running.

.PHONY: generate build test test-verbose initdb migrate-up migrate-down migrate-create seed setup db dev check help install-cc uninstall-cc

# Load DATABASE_URL from .env files (matches docker-compose defaults).
-include .env.development
-include .env.local
export

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

generate: ## Regenerate SQLC (no live DB required)
	cd db && sqlc generate

build: ## Typecheck the entire module
	go build -o /dev/null ./...

test: ## Run all tests
	go test ./...

test-verbose: ## Run all tests with verbose output
	go test -v ./...

initdb: ## Create the database and router schema (idempotent)
	@go run ./cmd/initdb

migrate-up: initdb ## Apply all pending migrations
	migrate -path db/migrations \
		-database "$(DATABASE_URL)&search_path=router" up

migrate-down: ## Roll back the last migration
	migrate -path db/migrations \
		-database "$(DATABASE_URL)&search_path=router" down 1

migrate-create: ## Create a new migration (usage: make migrate-create NAME=add-foo)
	@if [ -z "$(NAME)" ]; then echo "Usage: make migrate-create NAME=add-foo"; exit 1; fi
	migrate create -ext sql -dir db/migrations $(NAME)

seed: ## Create a local dev installation + API key and print usage instructions
	go run ./cmd/seed

setup: migrate-up seed ## Bootstrap: init DB, run migrations, seed an API key

db: ## Start the compose Postgres only (port 5433)
	docker compose up -d postgres
	@echo ""
	@echo "Postgres is running on localhost:5433."
	@echo "Add this to .env.local if not already set:"
	@echo '  DATABASE_URL=postgresql://router:router@localhost:5433/router?sslmode=disable'

dev: ## Run with hot-reload (CompileDaemon)
	# `-tags ORT` is required for hugot v0.7+ to enable the ONNX Runtime
	# backend. Without it, cluster.NewEmbedder fails at boot and the
	# router falls open to the heuristic (Anthropic-only) — which silently
	# breaks any eval that expects v0.X-cluster routing. The Dockerfile
	# already builds with this tag; do not drop it from any production-
	# bound build either. See router/CLAUDE.md "Cluster routing (P0)".
	#
	# CGO_LDFLAGS (libtokenizers) and ROUTER_ONNX_LIBRARY_DIR
	# (libonnxruntime) come from .env.local on macOS — see the comments
	# there for setup. On Linux the brew/.local paths don't apply; the
	# Dockerfile is the production path.
	CompileDaemon \
		-build="go build -tags ORT -o ./bin/server ./cmd/router" \
		-command="./bin/server" \
		-exclude-dir="vendor" \
		-exclude-dir=".vscode" \
		-exclude-dir="bin" \
		-exclude-dir=".venv" \
		-exclude-dir="__pycache__" \
		-exclude-dir=".pytest_cache" \
		-exclude-dir=".mypy_cache" \
		-exclude-dir=".ruff_cache" \
		-exclude-dir=".bench-cache" \
		-exclude-dir=".embedding-cache" \
		-exclude-dir="node_modules" \
		-exclude-dir="results" \
		-exclude-dir="logs" \
		-exclude-dir="assets" \
		-exclude-dir=".git" \
		-exclude-dir="eval" \
		-exclude-dir="scripts" \
		-exclude-dir="docs" \
		-exclude-dir=".local" \
		-exclude-dir="install" \
		-pattern="(.+\.go|.+\.sql)$$" \
		-graceful-kill=true \
		-log-prefix=false

install-cc: ## Point Claude Code at the local docker-compose router (no key needed)
	./install/install.sh --local

uninstall-cc: ## Remove the local Claude Code → router config
	./install/uninstall.sh

check: generate build test ## Full CI-equivalent check (generate + build + test)
	@if ! git diff --quiet internal/sqlc/; then \
		echo "error: sqlc generation produced uncommitted changes"; \
		git diff internal/sqlc/; \
		exit 1; \
	fi
	@echo "All checks passed."