#!/usr/bin/env bash
# Replay the most recent large inbound request from the local router's
# docker logs against the running router, repeatedly, to see if it
# reproduces the empty-assistant-turn pathology (upstream returning
# end_turn / stop with zero content blocks — observed in prod for
# qwen/qwen3.5-flash-02-23 on a ~360KB post-tool follow-up).
#
# Prereqs:
#   - Local router-server-1 docker container running on http://localhost:8080
#   - Recent CC traffic so docker logs has a large request to replay
#   - For full diagnostic output (raw upstream bytes in the WARN log) start
#     the router with WEAVE_ROUTER_DEBUG_EMPTY_RESPONSE=true. Without it
#     you still get the structured anomaly log line (model, provider,
#     request_id, estimated_input_tokens) but no upstream body capture.
#
# Usage: ./scripts/repro-empty-response.sh [iterations] [router_url] [api_key]
#   iterations defaults to 5
#   router_url defaults to http://localhost:8080/v1/messages
#   api_key defaults to $WEAVE_ROUTER_KEY

set -euo pipefail

ITER="${1:-5}"
URL="${2:-http://localhost:8080/v1/messages}"
KEY="${3:-${WEAVE_ROUTER_KEY:-}}"

if [[ -z "$KEY" ]]; then
  echo "ERROR: no API key. Set WEAVE_ROUTER_KEY env or pass as 3rd arg." >&2
  exit 2
fi

WORKDIR="$(mktemp -d -t repro-empty-XXXXXX)"
trap "rm -rf '$WORKDIR'" EXIT

echo "Finding most recent large inbound in router-server-1 logs..."
docker logs router-server-1 --since 4h 2>&1 \
  | sed -E 's/\x1b\[[0-9;]*m//g' \
  | grep "inbound anthropic request" \
  | grep -E "body_bytes=[0-9]{6,}" \
  | tail -1 > "$WORKDIR/inbound.line"

if [[ ! -s "$WORKDIR/inbound.line" ]]; then
  echo "ERROR: no large inbound request found in recent router logs." >&2
  echo "Generate one by using CC against the local router and trying a" >&2
  echo "prompt that reads a sizable file via a tool call." >&2
  exit 1
fi

# The body= field is wrapped in double quotes with inner quotes/backslashes
# escaped. Python's unicode_escape codec exactly inverts the encoder slog
# uses for this field, so this round-trip is lossless.
INBOUND_LINE="$WORKDIR/inbound.line" python3 <<'PY' > "$WORKDIR/body.json"
import os, re, json
with open(os.environ["INBOUND_LINE"]) as f:
    line = f.read()
m = re.search(r'body="(.*)"\s*$', line)
if not m:
    raise SystemExit("no body= field found in log line")
escaped = m.group(1)
unescaped = escaped.encode('utf-8').decode('unicode_escape')
parsed = json.loads(unescaped)
print(json.dumps(parsed))
PY

REQ_BYTES=$(wc -c < "$WORKDIR/body.json")
echo "Replaying request body: $REQ_BYTES bytes, $ITER iterations"
echo ""

EMPTY=0
for i in $(seq 1 "$ITER"); do
  RESP="$WORKDIR/resp-$i.sse"
  START=$(python3 -c 'import time; print(int(time.time()*1000))')
  curl -sS -N -X POST "$URL" \
    -H "Content-Type: application/json" \
    -H "Accept: text/event-stream" \
    -H "Anthropic-Version: 2023-06-01" \
    -H "X-Weave-Router-Key: $KEY" \
    -H "X-App: repro-empty-response" \
    --data-binary "@$WORKDIR/body.json" > "$RESP"
  END=$(python3 -c 'import time; print(int(time.time()*1000))')

  N_BLOCKS=$(grep -c "event: content_block_start" "$RESP" || true)
  STOP=$(grep -oE '"stop_reason":"[^"]*"' "$RESP" | head -1 | sed 's/"stop_reason":"//;s/"//')
  MODEL=$(grep -oE '"model":"[^"]*"' "$RESP" | head -1 | sed 's/"model":"//;s/"//')
  LATENCY_MS=$((END - START))

  if [[ "$N_BLOCKS" -eq 0 ]]; then
    echo "iter $i: ⚠️  EMPTY  (0 blocks, stop=$STOP, model=$MODEL, ${LATENCY_MS}ms) → $RESP"
    EMPTY=$((EMPTY + 1))
  else
    echo "iter $i: ✓ $N_BLOCKS blocks (stop=$STOP, model=$MODEL, ${LATENCY_MS}ms)"
  fi
done

echo ""
echo "Reproduced: $EMPTY/$ITER empty responses"
echo ""
echo "Inspect router-side anomaly logs:"
echo "  docker logs router-server-1 --since 5m 2>&1 \\"
echo "    | sed -E 's/\\x1b\\[[0-9;]*m//g' \\"
echo "    | grep -E 'Empty assistant turn|ProxyMessages complete' \\"
echo "    | tail -40"
echo ""
if [[ "$EMPTY" -gt 0 ]]; then
  echo "Empty responses preserved at: $RESP"
  echo "(WORKDIR will be deleted on exit — copy out anything you need)"
  # Keep the tmpdir around if anything went wrong so the user can inspect.
  trap - EXIT
  echo "WORKDIR retained: $WORKDIR"
fi
