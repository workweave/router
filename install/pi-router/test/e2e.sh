#!/usr/bin/env bash
#
# End-to-end test for @workweave/pi-router + the `install.sh --pi` target.
#
# Drives a REAL `pi` process against a mock Weave Router (mock_router.py) and
# asserts the routed-request shape the router actually receives: routing knobs,
# identity headers, and the metadata.user_id session/subagent signal -- for the
# main loop, for dispatch subagents, and for on-disk (key-file + models.json)
# resolution. No real model spend; no network beyond localhost.
#
# Requires: pi, jq, python3, curl. Run from anywhere:
#   install/pi-router/test/e2e.sh
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
INSTALL_SH="$(cd "$PKG_DIR/.." && pwd)/install.sh"
EXT="$PKG_DIR/src/index.ts"
MOCK="$SCRIPT_DIR/mock_router.py"

PORT="${MOCK_PORT:-8899}"
BASE_URL="http://127.0.0.1:$PORT"

for tool in pi jq python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FATAL: '$tool' not on PATH"; exit 2; }
done
[ -f "$EXT" ]        || { echo "FATAL: extension not found: $EXT"; exit 2; }
[ -f "$INSTALL_SH" ] || { echo "FATAL: installer not found: $INSTALL_SH"; exit 2; }
[ -f "$MOCK" ]       || { echo "FATAL: mock not found: $MOCK"; exit 2; }

WORK="$(mktemp -d)"
LOG="$WORK/requests.jsonl"
PI_DIR="$WORK/.pi"
MOCK_PID=""
KEEP_WORK=0

cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && wait "$MOCK_PID" 2>/dev/null || true
  if [ "$KEEP_WORK" = "1" ]; then
    echo "diagnostics preserved in $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# Deterministic identity + key. The mock accepts any key value.
export WEAVE_ROUTER_KEY="rk_e2e_testkey_abcd"
export WEAVE_USER_EMAIL="e2e@workweave.ai"
export WEAVE_USER_NAME="E2E Tester"

PASS=0
FAIL=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); }
phase() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

# jqcount <filter> -> number of JSONL records matching the boolean jq filter
jqcount() { jq -s "[.[] | select($1)] | length" "$LOG"; }

# with_timeout <secs> <cmd...>  (macOS has no coreutils `timeout`)
with_timeout() {
  local secs="$1"; shift
  "$@" &
  local pid=$!
  ( sleep "$secs"; kill "$pid" 2>/dev/null ) &
  local wd=$!
  disown "$wd" 2>/dev/null || true  # suppress the job-control "Terminated" line
  local rc=0
  wait "$pid" 2>/dev/null || rc=$?
  kill "$wd" 2>/dev/null || true
  return "$rc"
}

# -------------------------------------------------------------------------
# Start the mock router and wait for it to accept connections.
# -------------------------------------------------------------------------
MOCK_LOG="$LOG" MOCK_PORT="$PORT" python3 "$MOCK" &
MOCK_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "$BASE_URL/health" >/dev/null 2>&1 && break
  sleep 0.1
done
curl -fsS "$BASE_URL/health" >/dev/null 2>&1 || { echo "FATAL: mock did not come up on $BASE_URL"; exit 2; }

# -------------------------------------------------------------------------
phase "Phase 1 — installer (install.sh --pi)"
# -------------------------------------------------------------------------
bash "$INSTALL_SH" --pi --base-url "$BASE_URL" --dir "$WORK" >"$WORK/install.out" 2>&1 </dev/null || true

[ -f "$PI_DIR/models.json" ]       && ok "models.json written"  || bad "models.json missing (see $WORK/install.out)"
[ -f "$PI_DIR/settings.json" ]     && ok "settings.json written" || bad "settings.json missing"
[ -f "$PI_DIR/.weave_router_key" ] && ok "router key file written" || bad "key file missing"

jq -e --arg u "$BASE_URL" '.providers.weave.baseUrl == $u' "$PI_DIR/models.json" >/dev/null 2>&1 \
  && ok "models.json baseUrl = $BASE_URL (root, no /v1)" || bad "models.json baseUrl wrong (want root, no /v1)"
jq -e '.providers.weave.api == "anthropic-messages" and .providers.weave.authHeader == false' "$PI_DIR/models.json" >/dev/null 2>&1 \
  && ok "provider api=anthropic-messages, authHeader=false" || bad "provider api/authHeader wrong"
jq -e '.providers.weave.headers["x-weave-routing-alpha"] == "0.8" and .providers.weave.headers["x-weave-routing-speed-weight"] == "0.05"' "$PI_DIR/models.json" >/dev/null 2>&1 \
  && ok "models.json carries main-loop knobs (0.8 / 0.05)" || bad "models.json knobs wrong"
jq -e '.providers.weave.headers["X-Weave-User-Email"] == "e2e@workweave.ai"' "$PI_DIR/models.json" >/dev/null 2>&1 \
  && ok "identity baked into models.json headers" || bad "identity header missing in models.json"

PERM="$(stat -f '%Lp' "$PI_DIR/.weave_router_key" 2>/dev/null || stat -c '%a' "$PI_DIR/.weave_router_key" 2>/dev/null || echo '?')"
[ "$PERM" = "600" ] && ok "key file mode 600" || bad "key file mode $PERM (want 600)"

grep -q '"path":"/health"'   "$LOG" && ok "installer pinged /health"      || bad "no /health probe reached mock"
grep -q '"path":"/validate"' "$LOG" && ok "installer validated key (/validate)" || bad "no /validate probe reached mock"

# Idempotency: a second install must not duplicate the package entry.
bash "$INSTALL_SH" --pi --base-url "$BASE_URL" --dir "$WORK" >>"$WORK/install.out" 2>&1 </dev/null || true
PKGCOUNT="$(jq '[.packages[]? | select(. == "npm:@workweave/pi-router")] | length' "$PI_DIR/settings.json")"
[ "$PKGCOUNT" = "1" ] && ok "idempotent re-install: single pi-router package entry" || bad "package entry count = $PKGCOUNT (want 1)"

# The npm package is not published pre-merge; drop it so `-e` is the sole loader.
STRIPPED="$(jq 'del(.packages)' "$PI_DIR/settings.json")"
printf '%s\n' "$STRIPPED" >"$PI_DIR/settings.json"

# -------------------------------------------------------------------------
phase "Phase 2 — main-loop routing (real pi -p)"
# -------------------------------------------------------------------------
with_timeout 90 env PI_CODING_AGENT_DIR="$PI_DIR" \
  pi -e "$EXT" --no-session --offline --model weave/claude-sonnet-4-6 \
  -p "Say hello in three words." >"$WORK/main.out" 2>&1 </dev/null || true

if [ "$(jqcount '.method=="POST" and .app=="pi" and .path=="/v1/messages" and .rejected==false')" -ge 1 ]; then
  ok "pi sent a routed request to /v1/messages"
else
  bad "no valid main-loop request reached /v1/messages (see $WORK/main.out)"
fi
[ "$(jqcount '.app=="pi" and .knobs["x-weave-routing-alpha"]=="0.8" and .knobs["x-weave-routing-speed-weight"]=="0.05" and .knobs["x-weave-routing-output-cost-ratio"]=="0.5" and .knobs["x-weave-routing-expected-output-tokens"]=="3000"')" -ge 1 ] \
  && ok "main loop: quality knobs 0.8 / 0.05 / 0.5 / 3000" || bad "main-loop knobs wrong"
[ "$(jqcount '.app=="pi" and (.user_id // "" | startswith("pi:")) and (.user_id // "" | length) > 3')" -ge 1 ] \
  && ok "main loop: metadata.user_id = pi:<session>" || bad "main-loop user_id wrong"
[ "$(jqcount '.app=="pi" and .key_present==true')" -ge 1 ] \
  && ok "main loop: X-Weave-Router-Key forwarded" || bad "router key not forwarded"
[ "$(jqcount '.app=="pi" and .email=="e2e@workweave.ai"')" -ge 1 ] \
  && ok "main loop: identity email forwarded" || bad "identity email not forwarded"
grep -q "weave-routed-model: claude-opus-4-8" "$WORK/main.out" \
  && ok "x-router-model surfaced (headless stderr marker)" || bad "routed-model marker absent (see $WORK/main.out)"

# -------------------------------------------------------------------------
phase "Phase 3 — dispatch fan-out (real subagent processes)"
# -------------------------------------------------------------------------
with_timeout 120 env PI_CODING_AGENT_DIR="$PI_DIR" \
  pi -e "$EXT" --no-session --offline --model weave/claude-sonnet-4-6 \
  -p "__DISPATCH__ run two quick parallel checks." >"$WORK/dispatch.out" 2>&1 </dev/null || true

[ "$(jqcount '.app=="pi" and .served=="tool_use"')" -ge 1 ] \
  && ok "main loop invoked the dispatch tool" || bad "dispatch tool_use was never served (see $WORK/dispatch.out)"
SUBAGENT_REQS="$(jqcount '.app=="pi-subagent"')"
[ "$SUBAGENT_REQS" -ge 2 ] \
  && ok "spawned $SUBAGENT_REQS subagent requests (>=2 expected)" || bad "only $SUBAGENT_REQS subagent requests (want >=2)"
[ "$(jqcount '.app=="pi-subagent" and .knobs["x-weave-routing-alpha"]=="0.25" and .knobs["x-weave-routing-speed-weight"]=="0.45" and .knobs["x-weave-routing-output-cost-ratio"]=="2" and .knobs["x-weave-routing-expected-output-tokens"]=="1500"')" -ge 2 ] \
  && ok "subagents: speed/cheap knobs 0.25 / 0.45 / 2 / 1500" || bad "subagent knobs wrong"
[ "$(jqcount '.app=="pi-subagent" and (.user_id // "" | startswith("subagent:"))')" -ge 2 ] \
  && ok "subagents: metadata.user_id = subagent:<uuid>" || bad "subagent user_id wrong"
UNIQUE_SUBAGENT_IDS="$(jq -s '[.[] | select(.app=="pi-subagent") | .user_id] | unique | length' "$LOG")"
[ "$UNIQUE_SUBAGENT_IDS" -ge 2 ] \
  && ok "each subagent got a distinct session id ($UNIQUE_SUBAGENT_IDS unique)" || bad "subagent ids not distinct ($UNIQUE_SUBAGENT_IDS)"
[ "$(jqcount '.app=="pi" and .has_tool_result==true')" -ge 1 ] \
  && ok "main loop resumed after tool_result (loop terminated cleanly)" || bad "no post-dispatch main turn (loop did not complete)"

# -------------------------------------------------------------------------
phase "Phase 4 — on-disk resolution (no env; key file + models.json)"
# -------------------------------------------------------------------------
# Unset every WEAVE_* override so the extension MUST resolve the key from the
# installer-written key file and the base URL from models.json (the bug fix).
with_timeout 90 env -u WEAVE_ROUTER_KEY -u WEAVE_ROUTER_URL -u WEAVE_USER_EMAIL -u WEAVE_USER_NAME \
  PI_CODING_AGENT_DIR="$PI_DIR" \
  pi -e "$EXT" --no-session --offline --model weave/claude-sonnet-4-6 \
  -p "Resolve credentials from disk." >"$WORK/resolve.out" 2>&1 </dev/null || true

# Requests carrying our test key's suffix prove key-file + models.json resolution
# worked with no env vars set (the request reached the mock at all == baseUrl ok).
[ "$(jqcount '.app=="pi" and .key_present==true and .key_suffix=="abcd"')" -ge 1 ] \
  && ok "resolved key from key file + baseUrl from models.json (no env)" || bad "on-disk resolution failed (see $WORK/resolve.out)"

# -------------------------------------------------------------------------
phase "Result"
# -------------------------------------------------------------------------
# Endpoint correctness across every phase: the Anthropic SDK appends
# /v1/messages to baseUrl, so a baseUrl ending in /v1 yields /v1/v1/messages and
# 404s on the real router. Any rejected POST means a wrong baseUrl shipped.
WRONGPATH="$(jqcount '.method=="POST" and .rejected==true')"
[ "$WRONGPATH" -eq 0 ] \
  && ok "all routed POSTs hit /v1/messages (no /v1 doubling)" \
  || bad "$WRONGPATH POST(s) hit a non-/v1/messages path -> would 404 on the real router"

printf 'requests logged: %s\n' "$(wc -l <"$LOG" | tr -d ' ')"
printf '\033[1m%s passed, %s failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then KEEP_WORK=1; echo "FAILED"; exit 1; fi
echo "ALL GREEN"
