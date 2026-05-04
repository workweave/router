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
#   WEAVE ROUTER — claude-sonnet-4-5 ← claude-opus-4-7 · saved $0.04 turn / $1.23 session
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

# USD per 1k tokens. Source of truth: router/internal/observability/otel/pricing.go
# (USD/1M there, ÷1000 here). A Go test (TestStatuslinePricingMatchesTable)
# fails CI if these drift. Edit pricing.go and re-run the sync — do not
# hand-edit just this block.
# BEGIN_GENERATED_PRICES
prices='{
  "input": {
    "claude-opus-4-7":               0.015,
    "claude-sonnet-4-5":             0.003,
    "claude-haiku-4-5":              0.0008,
    "gpt-5.5":                       0.005,
    "gpt-5.5-pro":                   0.030,
    "gpt-5.5-mini":                  0.0005,
    "gpt-5.5-nano":                  0.00015,
    "gpt-5.4":                       0.003,
    "gpt-5.4-pro":                   0.020,
    "gpt-5.4-mini":                  0.0004,
    "gpt-5.4-nano":                  0.0001,
    "gpt-5":                         0.0025,
    "gpt-5-chat":                    0.0025,
    "gpt-5-mini":                    0.0005,
    "gpt-5-nano":                    0.0001,
    "gpt-4.1":                       0.002,
    "gpt-4.1-mini":                  0.0004,
    "gpt-4.1-nano":                  0.0001,
    "gpt-4o":                        0.0025,
    "gpt-4o-mini":                   0.00015,
    "gemini-3-pro-preview":          0.002,
    "gemini-3.1-pro-preview":        0.002,
    "gemini-3-flash-preview":        0.0005,
    "gemini-3.1-flash-lite-preview": 0.0001,
    "gemini-2.5-pro":                0.00125,
    "gemini-2.5-flash":              0.0003,
    "gemini-2.5-flash-lite":         0.0001,
    "gemini-2.0-flash":              0.0001,
    "gemini-2.0-flash-lite":         0.000075
  },
  "output": {
    "claude-opus-4-7":               0.075,
    "claude-sonnet-4-5":             0.015,
    "claude-haiku-4-5":              0.004,
    "gpt-5.5":                       0.040,
    "gpt-5.5-pro":                   0.120,
    "gpt-5.5-mini":                  0.0025,
    "gpt-5.5-nano":                  0.0006,
    "gpt-5.4":                       0.012,
    "gpt-5.4-pro":                   0.080,
    "gpt-5.4-mini":                  0.0016,
    "gpt-5.4-nano":                  0.0004,
    "gpt-5":                         0.010,
    "gpt-5-chat":                    0.010,
    "gpt-5-mini":                    0.002,
    "gpt-5-nano":                    0.0004,
    "gpt-4.1":                       0.008,
    "gpt-4.1-mini":                  0.0016,
    "gpt-4.1-nano":                  0.0004,
    "gpt-4o":                        0.010,
    "gpt-4o-mini":                   0.0006,
    "gemini-3-pro-preview":          0.008,
    "gemini-3.1-pro-preview":        0.008,
    "gemini-3-flash-preview":        0.002,
    "gemini-3.1-flash-lite-preview": 0.0004,
    "gemini-2.5-pro":                0.005,
    "gemini-2.5-flash":              0.0012,
    "gemini-2.5-flash-lite":         0.0004,
    "gemini-2.0-flash":              0.0004,
    "gemini-2.0-flash-lite":         0.0003
  }
}'
# END_GENERATED_PRICES

routed=""
turn_savings=""
session_savings=""

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

  # Compute savings across every assistant turn that the router actually
  # rerouted. cache_creation is priced at 1.25× input, cache_read at 0.1× —
  # both ratios are stable across the Claude family and a no-op when the
  # provider doesn't return those fields.
  read -r turn_savings session_savings < <(
    jq -r --argjson p "$prices" --argjson decisions "$decisions_map" '
      select(.type=="assistant") |
      .requestId as $req_id |
      ($decisions[$req_id] // null) as $requested_raw |
      .message as $m |
      ($m.model // "" | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $rm |
      # Emit one number per assistant turn so awk’s "last" tracks the
      # actually-most-recent turn, not the last *rerouted* turn.
      # Non-rerouted / unmeasurable turns emit 0.
      if $requested_raw == null then 0
      else
        ($requested_raw | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $requested |
        if $requested == $rm then 0
        else
          {
            in:    ($m.usage.input_tokens // 0),
            out:   ($m.usage.output_tokens // 0),
            cwrt:  ($m.usage.cache_creation_input_tokens // 0),
            crd:   ($m.usage.cache_read_input_tokens // 0)
          } as $t |
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
      end
    ' "$transcript_path" 2>/dev/null \
    | awk 'BEGIN{tot=0; last=0} {tot+=$1; last=$1} END{printf "%.4f %.4f\n", last, tot}'
  )
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

if [[ -n "$routed" ]]; then
  # Show the savings clause only when the session is genuinely net-saving.
  # session_savings is "0.0000" when no rerouted turns were found (sidecar
  # missing, fresh session, only side-calls so far); it can also go
  # negative when sticky routing forces a haiku-tagged side-call up to a
  # cached sonnet/opus decision. In both cases the word "saved" would
  # mislead, so drop the clause and just show the routed model.
  has_savings="false"
  if [[ -n "$session_savings" ]] \
     && awk -v v="$session_savings" 'BEGIN{exit !(v+0 > 0.005)}'; then
    has_savings="true"
  fi
  if [[ "$has_savings" == "true" ]]; then
    printf '%s — %s ← %s · saved %s turn / %s session' \
      "$brand" "$routed" "$selected_display" "$(fmt_money "$turn_savings")" "$(fmt_money "$session_savings")"
  else
    printf '%s — %s' "$brand" "$routed"
  fi
else
  printf '%s — %s' "$brand" "$selected_display"
fi
