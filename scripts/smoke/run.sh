#!/usr/bin/env bash
# Router pre-merge smoke suite orchestrator.
#
# Boots the router docker compose stack — plus the smoke suite's record/replay
# MITM proxy (smoke/mitmproxy/) sitting between the router and its upstream
# providers (Anthropic + OpenAI) — and runs the fixture-based smoke tests
# (smoke/ package, `smoke` build tag). Used by `make smoke` and
# .github/workflows/smoke.yml.
#
# The MITM proxy means most runs need NO provider API keys: PR CI defaults to
# SMOKE_PROXY_MODE=replay-only, reading the cassettes committed under
# smoke/mitmproxy/cassettes/. Keys are only needed to (re)record — locally, or
# in the nightly drift-check workflow.
#
# Env:
#   SMOKE_PROXY_MODE      replay-only (default) | record | replay-or-record
#                          record and replay-or-record need ANTHROPIC_API_KEY
#                          (OPENAI_API_KEY optional — only needed to record the
#                          gpt-5.x Responses-API scenarios; those are skipped
#                          without it, even in record mode).
#   ANTHROPIC_API_KEY     required only when SMOKE_PROXY_MODE != replay-only
#   OPENAI_API_KEY        optional; enables recording the OpenAI-path scenarios
#   SMOKE_KEEP_STACK=1    leave the stack running after the tests (local iteration)
#   SMOKE_PIN_MODEL       Anthropic model every scenario pins (default claude-haiku-4-5)
#   SMOKE_CI_CACHE=1      layer-cache the server/mitmproxy builds via the GitHub
#                          Actions cache backend (type=gha). Set only by
#                          .github/workflows/smoke.yml — type=gha hard-errors
#                          outside an actual GitHub Actions runner (it needs
#                          ACTIONS_CACHE_URL/ACTIONS_RUNTIME_TOKEN), so never set
#                          this locally.
#
# Cost: replay-only runs make zero upstream calls (served from cassettes).
# record/replay-or-record make ~10-15 real calls, all pinned to the cheapest
# tier of each provider with small max_tokens — a few cents.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

OVERRIDE_FILE="$REPO_ROOT/docker-compose.override.yml"
BASE_URL="${SMOKE_BASE_URL:-http://localhost:8080}"
PROXY_MODE="${SMOKE_PROXY_MODE:-replay-only}"
# docker compose only auto-loads docker-compose.override.yml when no -f flags
# are given at all; once we pass explicit -f files (for the mitmproxy overlay)
# it must be listed explicitly too, or the pubsub-port-drop below silently
# never applies and `up` fails on the 8085 conflict with the monorepo's own
# emulator (see .claude/skills/test-claude-locally).
COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.override.yml -f smoke/mitmproxy/docker-compose.yml)
if [[ "${SMOKE_CI_CACHE:-0}" == "1" ]]; then
  COMPOSE_FILES+=(-f smoke/mitmproxy/docker-compose.ci-cache.yml)
fi
COMPOSE="docker compose ${COMPOSE_FILES[*]}"

log() { printf '\n\033[1;36m[smoke]\033[0m %s\n' "$*"; }
err() { printf '\n\033[1;31m[smoke]\033[0m %s\n' "$*" >&2; }

case "$PROXY_MODE" in
  replay-only) ;;
  record|replay-or-record)
    if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
      err "SMOKE_PROXY_MODE=$PROXY_MODE needs ANTHROPIC_API_KEY (only replay-only runs key-free)."
      exit 2
    fi
    ;;
  *)
    err "invalid SMOKE_PROXY_MODE=$PROXY_MODE (want replay-only | record | replay-or-record)"
    exit 2
    ;;
esac

# The router itself must see non-empty provider keys to take the router-key
# dispatch path (cmd/router/main.go) rather than falling back to client-auth
# passthrough, which our harness doesn't send credentials for. In replay-only
# mode the MITM proxy never validates these — no request reaches the real
# provider — so placeholders are enough. record/replay-or-record need the real
# Anthropic key (checked above); OPENAI_API_KEY is optional even then — if
# unset, the OpenAI-path scenarios skip themselves (see smoke/openai_test.go).
SERVER_ANTHROPIC_KEY="${ANTHROPIC_API_KEY:-sk-ant-smoke-placeholder-unused-in-replay-only}"
SERVER_OPENAI_KEY="${OPENAI_API_KEY:-sk-smoke-placeholder-unused-in-replay-only}"

# The router server reads its own env from env_file only; docker compose does
# not pass host env into the container unless referenced. The override drops
# the pubsub host-port binding to sidestep an 8085 clash with the monorepo's
# own emulator (see .claude/skills/test-claude-locally). The real/placeholder
# keys above go on the server; the MITM proxy gets the real keys only via
# SMOKE_PROXY_MODE + ANTHROPIC_API_KEY/OPENAI_API_KEY (docker-compose.yml
# passes them through).
cleanup() {
  local code=$?
  if [[ $code -ne 0 ]]; then
    err "smoke run failed (exit $code); dumping server + mitmproxy logs:"
    $COMPOSE logs server mitmproxy --since=10m 2>&1 | sed -E 's/\x1b\[[0-9;]*m//g' | tail -150 || true
  fi
  rm -f "$OVERRIDE_FILE"
  if [[ "${SMOKE_KEEP_STACK:-0}" == "1" ]]; then
    log "SMOKE_KEEP_STACK=1 — leaving the stack up. Tear down with: $COMPOSE down -v"
  else
    $COMPOSE down -v >/dev/null 2>&1 || true
  fi
  exit $code
}
trap cleanup EXIT

log "writing ephemeral docker-compose.override.yml"
cat > "$OVERRIDE_FILE" <<EOF
services:
  pubsub-emulator:
    ports: !reset []
  server:
    environment:
      ANTHROPIC_API_KEY: "${SERVER_ANTHROPIC_KEY}"
      OPENAI_API_KEY: "${SERVER_OPENAI_KEY}"
EOF

log "building and starting the router stack (proxy mode: $PROXY_MODE)"
SMOKE_PROXY_MODE="$PROXY_MODE" $COMPOSE up -d --build server mitmproxy

log "waiting for /health at ${BASE_URL}"
deadline=$((SECONDS + 120))
until curl -sf "${BASE_URL}/health" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    err "router did not become healthy within 120s"
    exit 1
  fi
  sleep 2
done
log "router healthy"

log "seeding a router key"
SEED_OUTPUT="$($COMPOSE run --rm seed 2>/dev/null)"
ROUTER_KEY="$(printf '%s\n' "$SEED_OUTPUT" | grep -oE 'rk_[A-Za-z0-9_-]+' | head -1)"
if [[ -z "$ROUTER_KEY" ]]; then
  err "could not parse an rk_... key from the seed output:"
  printf '%s\n' "$SEED_OUTPUT" >&2
  exit 1
fi
log "seeded router key ${ROUTER_KEY:0:8}…"

log "running the smoke suite (proxy mode: $PROXY_MODE)"
SMOKE_ROUTER_KEY="$ROUTER_KEY" \
SMOKE_BASE_URL="$BASE_URL" \
SMOKE_OPENAI_ENABLED="$( [[ -n "${OPENAI_API_KEY:-}" || "$PROXY_MODE" == "replay-only" ]] && echo 1 || echo 0 )" \
  go test -tags smoke -count=1 -v ./smoke/

log "smoke suite passed"

if [[ "$PROXY_MODE" != "replay-only" ]]; then
  if ! git diff --quiet -- smoke/mitmproxy/cassettes/ 2>/dev/null || \
     [[ -n "$(git status --porcelain -- smoke/mitmproxy/cassettes/ 2>/dev/null)" ]]; then
    log "cassettes changed — review and commit smoke/mitmproxy/cassettes/ if this run should update the fixtures"
  fi
fi

