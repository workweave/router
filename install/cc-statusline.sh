#!/usr/bin/env bash
#
# Claude Code statusline for the Weave router. CC pipes a JSON blob on stdin
# whose `transcript_path` points at the JSONL log of the current session. The
# router rewrites each request's `model` field before forwarding, so
# Anthropic/OpenAI/Google return `message.model = <routed>` in the SSE stream
# and CC stores that in the transcript verbatim. We also read each turn's
# `message.usage` to compare what the routed call actually cost against what
# the user-selected model would have cost on the same tokens.
#
# Wire up by adding to ~/.claude/settings.json:
#   { "statusLine": { "type": "command", "command": "/abs/path/to/cc-statusline.sh" } }
#
# Renders:
#   WEAVE ROUTER — claude-sonnet-4-5 ← claude-opus-4-7 · saved $1.23 · 12.4k in / 3.1k out / 45.2k cached
#
# Pricing source of truth: router/eval/pricing.py. Keep these maps in lockstep
# when prices change. Cache multipliers (1.25× / 0.1×) follow Anthropic's
# published cache pricing and are stable across the Claude family.

set -euo pipefail

input="$(cat)"
transcript_path="$(printf '%s' "$input" | jq -r '.transcript_path // empty')"
selected_display="$(printf '%s' "$input" | jq -r '.model.display_name // .model.id // "?"')"

# Normalize a model id to a pricing-table key. CC + the decisions log carry
# two flavors of annotation we don't want in the lookup:
#   * date suffix:    claude-opus-4-7-20260101  → claude-opus-4-7
#   * variant tag:    claude-opus-4-7[1m]       → claude-opus-4-7
# The 1M-context variant prices ~2× base for prompts >200k tokens, but for
# the "saved $X vs your selection" UX the base rate is the right comparison
# — we're measuring the model swap, not the context tier. Used below on the
# routed and requested model ids from the decisions log / transcript.
normalize_model() {
  printf '%s' "$1" | sed -E 's/\[[^]]*\]$//; s/-[0-9]{8}$//'
}

# USD per 1k tokens. Generated from internal/observability/otel/pricing.go
# (USD/1M there, ÷1000 here) by cmd/genprices. Do not hand-edit — run
# `make generate` after updating pricing.go.
# BEGIN_GENERATED_PRICES
prices='{
  "input": {
    "claude-haiku-4-5":                 0.0008,
    "claude-opus-4-7":                  0.015,
    "claude-sonnet-4-5":                0.003,
    "deepseek/deepseek-v4-flash":       0.00014,
    "deepseek/deepseek-v4-pro":         0.000435,
    "gemini-2.0-flash":                 0.0001,
    "gemini-2.0-flash-lite":            0.000075,
    "gemini-2.5-flash":                 0.0003,
    "gemini-2.5-flash-lite":            0.0001,
    "gemini-2.5-pro":                   0.00125,
    "gemini-3-flash-preview":           0.0005,
    "gemini-3-pro-preview":             0.002,
    "gemini-3.1-flash-lite-preview":    0.0001,
    "gemini-3.1-pro-preview":           0.002,
    "gpt-4.1":                          0.002,
    "gpt-4.1-mini":                     0.0004,
    "gpt-4.1-nano":                     0.0001,
    "gpt-4o":                           0.0025,
    "gpt-4o-mini":                      0.00015,
    "gpt-5":                            0.0025,
    "gpt-5-chat":                       0.0025,
    "gpt-5-mini":                       0.0005,
    "gpt-5-nano":                       0.0001,
    "gpt-5.4":                          0.003,
    "gpt-5.4-mini":                     0.0004,
    "gpt-5.4-nano":                     0.0001,
    "gpt-5.4-pro":                      0.02,
    "gpt-5.5":                          0.005,
    "gpt-5.5-mini":                     0.0005,
    "gpt-5.5-nano":                     0.00015,
    "gpt-5.5-pro":                      0.03,
    "mistralai/mistral-small-2603":     0.00015,
    "moonshotai/kimi-k2.5":             0.00044,
    "qwen/qwen3-235b-a22b-2507":        0.000071,
    "qwen/qwen3-30b-a3b-instruct-2507": 0.00008,
    "qwen/qwen3-coder":                 0.00022,
    "qwen/qwen3-coder-next":            0.00007,
    "qwen/qwen3-next-80b-a3b-instruct": 0.00009,
    "qwen/qwen3.5-flash-02-23":         0.000065
  },
  "output": {
    "claude-haiku-4-5":                 0.004,
    "claude-opus-4-7":                  0.075,
    "claude-sonnet-4-5":                0.015,
    "deepseek/deepseek-v4-flash":       0.00028,
    "deepseek/deepseek-v4-pro":         0.00087,
    "gemini-2.0-flash":                 0.0004,
    "gemini-2.0-flash-lite":            0.0003,
    "gemini-2.5-flash":                 0.0012,
    "gemini-2.5-flash-lite":            0.0004,
    "gemini-2.5-pro":                   0.005,
    "gemini-3-flash-preview":           0.002,
    "gemini-3-pro-preview":             0.008,
    "gemini-3.1-flash-lite-preview":    0.0004,
    "gemini-3.1-pro-preview":           0.008,
    "gpt-4.1":                          0.008,
    "gpt-4.1-mini":                     0.0016,
    "gpt-4.1-nano":                     0.0004,
    "gpt-4o":                           0.01,
    "gpt-4o-mini":                      0.0006,
    "gpt-5":                            0.01,
    "gpt-5-chat":                       0.01,
    "gpt-5-mini":                       0.002,
    "gpt-5-nano":                       0.0004,
    "gpt-5.4":                          0.012,
    "gpt-5.4-mini":                     0.0016,
    "gpt-5.4-nano":                     0.0004,
    "gpt-5.4-pro":                      0.08,
    "gpt-5.5":                          0.04,
    "gpt-5.5-mini":                     0.0025,
    "gpt-5.5-nano":                     0.0006,
    "gpt-5.5-pro":                      0.12,
    "mistralai/mistral-small-2603":     0.0006,
    "moonshotai/kimi-k2.5":             0.002,
    "qwen/qwen3-235b-a22b-2507":        0.000463,
    "qwen/qwen3-30b-a3b-instruct-2507": 0.00033,
    "qwen/qwen3-coder":                 0.0018,
    "qwen/qwen3-coder-next":            0.0003,
    "qwen/qwen3-next-80b-a3b-instruct": 0.0011,
    "qwen/qwen3.5-flash-02-23":         0.00026
  }
}'
# END_GENERATED_PRICES

routed=""
session_savings=""
tot_in=0
tot_out=0
tot_cached=0

# Sidecar log written by the router with one JSON line per request:
# {ts, request_id, requested_model, decision_model, decision_reason,
#  decision_provider}. The script joins this against the transcript by
# request_id so we know each turn's *original* requested model — info
# the transcript itself doesn't carry. Without this we can't tell CC's
# haiku-tagged background side-calls from genuine opus-routed-down
# turns; both end up as message.model = claude-haiku-4-5 in the JSONL.
decisions_log="${ROUTER_DECISIONS_LOG_PATH:-$HOME/.weave-router/decisions.jsonl}"

if [[ -n "$transcript_path" && -f "$transcript_path" ]]; then
  # macOS ships `tail -r`, GNU coreutils ships `tac`. Either works to walk the
  # JSONL in reverse so we can grab the latest assistant turn.
  if command -v tac >/dev/null 2>&1; then reverse=(tac); else reverse=(tail -r); fi

  routed="$("${reverse[@]}" "$transcript_path" 2>/dev/null \
    | jq -r 'select(.type=="assistant") | .message.model // empty' \
    | head -n 1 || true)"
  routed="$(normalize_model "$routed")"

  # Build {request_id: requested_model} map from the sidecar log. Empty
  # object when the log is missing — savings calc then sees no entries
  # and emits no credit, which is the honest fallback. Bound the read at
  # the most recent ~5k lines so the per-render cost stays flat as the
  # log grows; CC re-renders on every state change and the log is
  # append-only with no rotation. Older entries can't match request_ids
  # in the current session anyway.
  decisions_map='{}'
  if [[ -f "$decisions_log" ]]; then
    decisions_map="$(tail -n 5000 "$decisions_log" 2>/dev/null \
      | jq -s 'map({(.request_id): .requested_model}) | add // {}' 2>/dev/null \
      || echo '{}')"
  fi

  # Compute a session running total: savings across every assistant turn the
  # router rerouted, plus cumulative token counts across every assistant turn
  # (whether rerouted or not — total work the session has done).
  # cache_creation is priced at 1.25× input, cache_read at 0.1× — both ratios
  # are stable across the Claude family and a no-op when the provider doesn't
  # return those fields. Cache reads ARE included in the savings comparison:
  # both the routed and would-have-been-selected costs apply the same 0.1×
  # weight to cache_read_input_tokens, so the delta reflects the model-price
  # difference on the cached portion as well.
  read -r session_savings tot_in tot_out tot_cached < <(
    jq -r --argjson p "$prices" --argjson decisions "$decisions_map" '
      select(.type=="assistant") |
      (.requestId // null) as $req_id |
      (if $req_id == null then null else ($decisions[$req_id] // null) end) as $requested_raw |
      .message as $m |
      ($m.model // "" | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $rm |
      {
        in:    ($m.usage.input_tokens // 0),
        out:   ($m.usage.output_tokens // 0),
        cwrt:  ($m.usage.cache_creation_input_tokens // 0),
        crd:   ($m.usage.cache_read_input_tokens // 0)
      } as $t |
      (if $requested_raw == null then 0
       else
         ($requested_raw | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $requested |
         if $requested == $rm then 0
         else
           ($p.input[$rm] // null)        as $rin  | ($p.output[$rm] // null)        as $rout |
           ($p.input[$requested] // null) as $sin  | ($p.output[$requested] // null) as $sout |
           if ($rin == null or $rout == null or $sin == null or $sout == null) then 0
           else
             (($t.in + 1.25 * $t.cwrt + 0.1 * $t.crd) / 1000) as $input_units |
             ($t.out / 1000)                                  as $output_units |
             ($input_units * $rin + $output_units * $rout)    as $routed_cost |
             ($input_units * $sin + $output_units * $sout)    as $requested_cost |
             ($requested_cost - $routed_cost)
           end
         end
       end) as $savings |
      "\($savings) \($t.in) \($t.out) \($t.cwrt + $t.crd)"
    ' "$transcript_path" 2>/dev/null \
    | awk 'BEGIN{s=0; i=0; o=0; c=0}
           {s+=$1; i+=$2; o+=$3; c+=$4}
           END{printf "%.4f %d %d %d\n", s, i, o, c}'
  ) || true
fi

# Brand color (#FF6C47) on terminals that grok 24-bit truecolor — that's
# every modern one (iTerm2, Apple Terminal, vscode, ghostty, alacritty,
# wezterm, kitty). Falls back gracefully on any escape-stripping terminal.
brand=$'\033[38;2;255;108;71mWEAVE ROUTER\033[0m'

# Format helpers.
fmt_money() {
  awk -v v="$1" 'BEGIN{
    if (v == "" || v+0 == 0)        { printf "$0.00";        exit }
    if (v+0 < 0.005 && v+0 > -0.005){ printf "<$0.01";       exit }
    if (v+0 < 0)                    { printf "-$%.2f", -v+0; exit }
    printf "$%.2f", v
  }'
}

fmt_tok() {
  awk -v v="$1" 'BEGIN{
    v = v+0
    if (v >= 1000000) { printf "%.1fM", v/1000000; exit }
    if (v >= 1000)    { printf "%.1fk", v/1000;    exit }
    printf "%d", v
  }'
}

tokens_clause=""
if [[ "$tot_in" -gt 0 || "$tot_out" -gt 0 || "$tot_cached" -gt 0 ]]; then
  tokens_clause=" · $(fmt_tok "$tot_in") in / $(fmt_tok "$tot_out") out / $(fmt_tok "$tot_cached") cached"
fi

if [[ -n "$routed" ]]; then
  # Show the savings clause only when the session is genuinely net-saving.
  # session_savings is "0.0000" when no rerouted turns were found (sidecar
  # missing, fresh session, only side-calls so far); it can also go
  # negative when sticky routing forces a haiku-tagged side-call up to a
  # cached sonnet/opus decision. In both cases the word "saved" would
  # mislead, so drop the savings clause but keep the token totals.
  has_savings="false"
  if [[ -n "$session_savings" ]] \
     && awk -v v="$session_savings" 'BEGIN{exit !(v+0 > 0.005)}'; then
    has_savings="true"
  fi
  if [[ "$has_savings" == "true" ]]; then
    printf '%s — %s ← %s · saved %s%s' \
      "$brand" "$routed" "$selected_display" "$(fmt_money "$session_savings")" "$tokens_clause"
  else
    printf '%s — %s%s' "$brand" "$routed" "$tokens_clause"
  fi
else
  printf '%s — %s%s' "$brand" "$selected_display" "$tokens_clause"
fi
