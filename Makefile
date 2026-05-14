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

.PHONY: generate generate-statusline build test test-verbose initdb migrate-up migrate-down migrate-create seed setup full-setup db dev check help install-cc uninstall-cc up down logs

# Load DATABASE_URL from .env files (matches docker-compose defaults).
-include .env.development
-include .env.local
export

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'

generate: generate-statusline ## Regenerate all generated files (SQLC + statusline prices)
	cd db && sqlc generate

generate-statusline: ## Sync cc-statusline.sh prices block from pricing.go
	go run ./cmd/genprices

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

setup: migrate-up seed ## Bootstrap (host DB): init DB, run migrations, seed an API key

full-setup: generate-statusline ## Bootstrap router: docker compose + seed + interactively wire Claude Code
	## Usage:
	##   make full-setup                                  # boot compose, seed, wire Claude Code (interactive scope prompt)
	##   make full-setup KEY=rk_... BASE_URL=http://...   # wire an already-running router to Claude Code
	##   make full-setup KEY=rk_... BASE_URL=http://... DIR=/tmp/test         # isolated throwaway install
	##   make full-setup KEY=rk_... BASE_URL=http://... SCOPE=user            # skip the interactive scope prompt
	##   make full-setup KEY=rk_... BASE_URL=http://... SCOPE=user NON_INTERACTIVE=1  # CI-friendly
	##
	## Cursor wiring is manual — see README "Wiring Claude Code or Cursor".
	@if [ -n "$(KEY)" ] && [ -n "$(BASE_URL)" ]; then \
		INSTALL_CMD='WEAVE_ROUTER_KEY="$(KEY)" ./install/install.sh --base-url "$(BASE_URL)"'; \
		[ -n "$(SCOPE)" ] && INSTALL_CMD="$$INSTALL_CMD --scope $(SCOPE)"; \
		[ -n "$(DIR)" ] && INSTALL_CMD="$$INSTALL_CMD --dir $(DIR)"; \
		[ "$(NON_INTERACTIVE)" = "1" ] && INSTALL_CMD="$$INSTALL_CMD --non-interactive"; \
		echo "==> Wiring Claude Code → $(BASE_URL)..."; \
		eval "$$INSTALL_CMD"; \
	else \
		if [ -n "$(KEY)" ] || [ -n "$(BASE_URL)" ]; then \
			echo "error: KEY and BASE_URL must both be provided together."; \
			exit 1; \
		fi; \
		COMPOSE_LOG="$$(mktemp -t full-setup-compose.XXXXXX.log)"; \
		echo "==> Building and starting docker compose stack (postgres, migrate, server)..."; \
		echo "    (build log: $$COMPOSE_LOG)"; \
		if ! docker compose up --build -d >"$$COMPOSE_LOG" 2>&1; then \
			echo "error: docker compose failed. Output:"; \
			cat "$$COMPOSE_LOG"; \
			exit 1; \
		fi; \
		echo "==> Waiting for the router to become healthy..."; \
		for i in $$(seq 1 60); do \
			if curl -fsS --max-time 2 http://localhost:8080/health >/dev/null 2>&1; then \
				echo "    healthy after $${i}s"; \
				break; \
			fi; \
			if [ "$$i" = "60" ]; then \
				echo "error: router did not become healthy within 60s. Tail with 'make logs'."; \
				exit 1; \
			fi; \
			sleep 1; \
		done; \
		echo "==> Seeding a Weave Router key for your installation..."; \
		SEED_OUTPUT=$$(docker compose run --rm seed 2>&1); \
		WEAVE_KEY=$$(echo "$$SEED_OUTPUT" | grep -oE "^  rk_[a-zA-Z0-9_-]+$$" | head -1 | xargs); \
		if [ -z "$$WEAVE_KEY" ]; then \
			echo "error: failed to extract router key from seed output."; \
			echo "$$SEED_OUTPUT"; \
			exit 1; \
		fi; \
		echo "    key: $$WEAVE_KEY"; \
		echo ""; \
		echo "==> Wiring Claude Code → router (you'll be prompted for scope: user or project)..."; \
		WEAVE_ROUTER_KEY="$$WEAVE_KEY" ./install/install.sh --base-url http://localhost:8080 --quiet; \
		echo ""; \
		echo "Done. Router on http://localhost:8080. Share with teammates: make full-setup KEY=$$WEAVE_KEY BASE_URL=<reachable-url>"; \
	fi

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

up: ## Start the compose stack in the background (no install.sh wiring)
	docker compose up --build -d

down: ## Stop the compose stack (keeps the postgres volume)
	docker compose down

logs: ## Tail the server logs
	docker compose logs -f server

install-cc: generate-statusline ## Wire only Claude Code at the local docker-compose router (assumes it's already running)
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