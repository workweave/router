#!/usr/bin/env bash
#
# Endpoint smoke test for the `install.sh --opencode` target.
#
# opencode routes through a DIFFERENT SDK than pi: @ai-sdk/anthropic (Vercel),
# which appends only /messages to baseURL — so opencode's config correctly keeps
# the /v1 suffix (baseURL = <url>/v1 -> /v1/messages). This is the OPPOSITE of
# pi's @anthropic-ai/sdk, which appends /v1/messages to a root baseURL. This
# guard proves opencode keeps landing on /v1/messages so nobody "aligns" the two
# baseURLs and breaks one. Reuses mock_router.py (404s any path != /v1/messages).
#
# Requires: opencode, jq, python3, curl. Run from anywhere:
#   install/pi-router/test/opencode_smoke.sh
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_SH="$(cd "$SCRIPT_DIR/../.." && pwd)/install.sh"
MOCK="$SCRIPT_DIR/mock_router.py"

PORT="${MOCK_PORT:-8911}"
BASE_URL="http://127.0.0.1:$PORT"

for tool in opencode jq python3 curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FATAL: '$tool' not on PATH"; exit 2; }
done
[ -f "$INSTALL_SH" ] || { echo "FATAL: installer not found: $INSTALL_SH"; exit 2; }
[ -f "$MOCK" ]       || { echo "FATAL: mock not found: $MOCK"; exit 2; }

WORK="$(mktemp -d)"
LOG="$WORK/requests.jsonl"
MOCK_PID=""
KEEP_WORK=0
cleanup() {
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  [ -n "$MOCK_PID" ] && wait "$MOCK_PID" 2>/dev/null || true
  [ "$KEEP_WORK" = "1" ] && echo "diagnostics preserved in $WORK" || rm -rf "$WORK"
}
trap cleanup EXIT

export WEAVE_ROUTER_KEY="rk_oc_smoke_key"
export WEAVE_USER_EMAIL="oc@workweave.ai"
export WEAVE_USER_NAME="OC Smoke"

PASS=0; FAIL=0
ok()  { printf '  \033[32mPASS\033[0m %s\n' "$1"; PASS=$((PASS + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); }
jqcount() { jq -s "[.[] | select($1)] | length" "$LOG"; }

MOCK_LOG="$LOG" MOCK_PORT="$PORT" python3 "$MOCK" &
MOCK_PID=$!
for _ in $(seq 1 50); do curl -fsS "$BASE_URL/health" >/dev/null 2>&1 && break; sleep 0.1; done
curl -fsS "$BASE_URL/health" >/dev/null 2>&1 || { echo "FATAL: mock did not come up"; exit 2; }

printf '\033[1m== install (install.sh --opencode --dir) ==\033[0m\n'
bash "$INSTALL_SH" --opencode --base-url "$BASE_URL" --dir "$WORK" >"$WORK/install.out" 2>&1 </dev/null || true
jq -e --arg u "$BASE_URL/v1" '.provider.weave.options.baseURL == $u' "$WORK/opencode.json" >/dev/null 2>&1 \
  && ok "opencode.json baseURL = $BASE_URL/v1 (Vercel SDK convention — keeps /v1)" \
  || bad "opencode.json baseURL wrong (see $WORK/install.out)"
jq -e '.provider.weave.npm == "@ai-sdk/anthropic"' "$WORK/opencode.json" >/dev/null 2>&1 \
  && ok "opencode provider uses @ai-sdk/anthropic" || bad "opencode provider npm wrong"

printf '\033[1m== run (opencode run, headless, isolated XDG) ==\033[0m\n'
mkdir -p "$WORK/xdg"
(
  cd "$WORK" || exit 1
  XDG_CONFIG_HOME="$WORK/xdg" opencode run "say hi in three words" >"$WORK/oc.out" 2>&1 </dev/null
) &
RPID=$!
( sleep 150; kill "$RPID" 2>/dev/null ) & WD=$!
disown "$WD" 2>/dev/null || true
wait "$RPID" 2>/dev/null || true
kill "$WD" 2>/dev/null || true

[ "$(jqcount '.method=="POST" and .app=="opencode" and .path=="/v1/messages" and .rejected==false')" -ge 1 ] \
  && ok "opencode hit /v1/messages (served, app=opencode)" \
  || bad "opencode did not reach /v1/messages (see $WORK/oc.out)"
WRONG="$(jqcount '.method=="POST" and .rejected==true')"
[ "$WRONG" -eq 0 ] \
  && ok "no /v1 doubling — every opencode POST hit /v1/messages" \
  || bad "$WRONG opencode POST(s) hit a wrong path -> would 404 on the real router"

printf '\033[1m== Result ==\033[0m\n'
printf '\033[1m%s passed, %s failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -ne 0 ]; then KEEP_WORK=1; echo "FAILED"; exit 1; fi
echo "ALL GREEN"
