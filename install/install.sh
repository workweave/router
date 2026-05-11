#!/usr/bin/env bash
#
# Weave Router installer for Claude Code.
#
# Configures Claude Code to permanently route through the Weave Router by
# writing the router base URL, router auth header, and a status line into
# Claude Code's settings.json. After running, `claude` Just Works — no shell
# exports, no manual settings edits.
#
# Two scopes:
#   - user (default):  ~/.claude/settings.json  + ~/.weave/cc-statusline.sh
#   - project:         <repo>/.claude/settings.json + <repo>/.claude/cc-statusline.sh
#
# Or pass --dir to install into any directory:
#   - dir:              <dir>/.claude/settings.json + <dir>/.claude/cc-statusline.sh
#
# Usage:
#   ./install.sh                                  # hosted router, user scope
#   ./install.sh --scope project                  # commit-with-team install
#   ./install.sh --dir /tmp/my-sandbox            # isolated throwaway install
#   ./install.sh --local                          # docker-compose router on localhost:8082, dev-mode
#   ./install.sh --base-url http://localhost:8082 # self-hosted, custom port / non-dev-mode
#   ./install.sh --non-interactive                # require WEAVE_ROUTER_KEY env var
#
#   curl -fsSL https://weave.ai/cc/install.sh | sh
#   curl -fsSL https://weave.ai/cc/install.sh | sh -s -- --scope project

set -euo pipefail

# ---------- defaults ----------

# The hosted Weave Router URL. Override with --base-url for self-hosted.
DEFAULT_BASE_URL="${WEAVE_ROUTER_URL:-https://router.weave.ai}"


scope="user"
scope_explicit="false"
install_dir=""
base_url=""
non_interactive="false"
dev_mode="false"
router_key_header="X-Weave-Router-Key"

# ---------- helpers ----------

err()  { printf "\033[31merror:\033[0m %s\n" "$*" >&2; }
warn() { printf "\033[33mwarning:\033[0m %s\n" "$*" >&2; }
info() { printf "\033[36m==>\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m✓\033[0m %s\n" "$*"; }

usage() {
  # Print the leading comment block (lines 2..just-before `set -euo`), stripping
  # the leading `# `. awk avoids GNU `head -n -<N>`, which BSD head on macOS
  # rejects with "illegal line count -- -N".
  awk 'NR<2 { next } /^set -euo/ { exit } { sub(/^# ?/, ""); print }' "$0"
  exit "${1:-0}"
}

require_cmd() {
  local cmd="$1" hint="$2"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    err "$cmd is required but not installed."
    printf "  install: %s\n" "$hint" >&2
    exit 1
  fi
}

# Refuse to write through a symlink. Project scope reads the install path from
# the user's git repo; a malicious checkout could ship `.claude/settings.json`
# (or `.claude/` itself) as a symlink to e.g. `~/.ssh/authorized_keys`, and
# the installer's mkdir/chmod/cp/jq>file would silently follow that link.
refuse_if_symlink() {
  local target="$1"
  if [ -L "$target" ]; then
    err "$target is a symlink (-> $(readlink "$target")). Refusing to write through it."
    exit 1
  fi
}

# ---------- arg parsing ----------

while [ $# -gt 0 ]; do
  case "$1" in
    --scope)
      scope="${2:-}"; shift 2
      [ "$scope" = "user" ] || [ "$scope" = "project" ] || { err "--scope must be 'user' or 'project'"; exit 2; }
      scope_explicit="true"
      ;;
    --base-url)
      base_url="${2:-}"; shift 2
      [ -n "$base_url" ] || { err "--base-url requires a value"; exit 2; }
      ;;
    --dev-mode)
      dev_mode="true"; shift
      ;;
    --local)
      # Shorthand for the docker-compose default: ROUTER_DEV_MODE=true on
      # localhost:8082 (matches router/docker-compose.yml). No key required.
      base_url="http://localhost:8082"
      dev_mode="true"
      shift
      ;;
    --non-interactive)
      non_interactive="true"; shift
      ;;
    --dir)
      install_dir="${2:-}"; shift 2
      [ -n "$install_dir" ] || { err "--dir requires a path"; exit 2; }
      ;;
    -h|--help)
      usage 0
      ;;
    *)
      err "unknown flag: $1"; usage 2
      ;;
  esac
done

if [ -z "$base_url" ]; then
  base_url="$DEFAULT_BASE_URL"
fi
# trim trailing slash for cleanliness
base_url="${base_url%/}"

# ---------- interactive scope prompt ----------

# If the user didn't pass --scope and we have a controlling terminal, ask which
# scope to install into. Non-interactive runs (CI, `curl | sh --non-interactive`)
# silently use the "user" default.
if [ -z "$install_dir" ] && [ "$scope_explicit" = "false" ] && [ "$non_interactive" = "false" ] && [ -r /dev/tty ]; then
  printf "Install scope:\n"
  printf "  1) user     — write to ~/.claude/ (applies everywhere you run claude)\n"
  printf "  2) project  — write to <repo>/.claude/ (applies only inside this repo)\n"
  printf "Choose [1/2] (default 1): "
  read -r scope_choice </dev/tty || scope_choice=""
  case "${scope_choice:-1}" in
    1|""|user|u|U)    scope="user" ;;
    2|project|p|P)    scope="project" ;;
    *) err "invalid choice: $scope_choice"; exit 2 ;;
  esac
fi

# ---------- pre-flight ----------

info "Weave Router installer (scope=$scope, base_url=$base_url)"

require_cmd jq    "macOS: 'brew install jq' · Debian/Ubuntu: 'sudo apt install jq'"
require_cmd curl  "macOS/Linux: usually preinstalled — check your package manager"

if ! command -v claude >/dev/null 2>&1; then
  warn "'claude' not found on PATH. Install Claude Code from https://claude.com/code, then re-run this script."
  warn "Continuing — settings.json will be written and will take effect once Claude Code is installed."
fi

script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"

# Resolve the base directory. User scope always uses $HOME. Project scope uses
# --dir if given, otherwise the CWD's git root. --dir alone (no --scope) is a
# throwaway user-style install.
if [ -n "$install_dir" ]; then
  install_dir="$(cd "$install_dir" 2>/dev/null && pwd || echo "$install_dir")"
  settings_base="$install_dir"
else
  case "$scope" in
    user)
      settings_base="$HOME"
      ;;
    project)
      if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
        err "--scope project must be run inside a git repo, or pass --dir <path>. cd into your project first, or use --dir."
        exit 1
      fi
      settings_base="$git_root"
      ;;
  esac
fi

case "$scope" in
  user)
    settings_dir="$settings_base/.claude"
    settings_file="$settings_dir/settings.json"
    local_settings_file=""
    statusline_dir="${settings_base}/.weave"
    statusline_file="$statusline_dir/cc-statusline.sh"
    statusline_path_for_settings="$statusline_file"
    ;;
  project)
    settings_dir="$settings_base/.claude"
    settings_file="$settings_dir/settings.json"
    local_settings_file="$settings_dir/settings.local.json"
    statusline_dir="$settings_base/.claude"
    statusline_file="$statusline_dir/cc-statusline.sh"
    # Portable relative path for real repos (teammates can clone anywhere).
    # Absolute path when --dir overrides (no meaningful $CLAUDE_PROJECT_DIR).
    if [ -z "$install_dir" ]; then
      statusline_path_for_settings="\${CLAUDE_PROJECT_DIR}/.claude/cc-statusline.sh"
    else
      statusline_path_for_settings="$statusline_file"
    fi
    ;;
esac

# Symlink containment: refuse if any target path is a symlink. User-scope paths
# under $HOME are trusted; project-scope and --dir paths come from a git repo or
# user-supplied directory that may be hostile, so we check those.
if [ "$scope" = "project" ] || [ -n "$install_dir" ]; then
  refuse_if_symlink "$settings_dir"
  refuse_if_symlink "$settings_file"
  refuse_if_symlink "$local_settings_file"
  refuse_if_symlink "$statusline_file"
fi

mkdir -p "$settings_dir" "$statusline_dir"

# ---------- token handling ----------

api_key=""
if [ "$dev_mode" = "true" ]; then
  info "Dev mode — skipping API key (router has ROUTER_DEV_MODE=true)."
else
  if [ -n "${WEAVE_ROUTER_KEY:-}" ]; then
    api_key="$WEAVE_ROUTER_KEY"
    info "Using WEAVE_ROUTER_KEY from environment."
  elif [ "$non_interactive" = "true" ]; then
    err "--non-interactive set but WEAVE_ROUTER_KEY is empty. Export it and re-run."
    exit 1
  else
    # Read from /dev/tty explicitly so the prompt works under `curl -fsSL ... | sh`,
    # where stdin is the curl pipe (already at EOF by the time we get here, and
    # `set -e` would abort on read returning 1). If /dev/tty isn't available
    # (e.g. CI without a controlling terminal) the user must use --non-interactive.
    if [ ! -r /dev/tty ]; then
      err "no controlling terminal — set WEAVE_ROUTER_KEY and re-run with --non-interactive."
      exit 1
    fi
    # Restore terminal echo on any exit path (Ctrl-C, error, signal). Without
    # this trap, an interrupted prompt leaves the user's terminal stuck silent.
    trap 'stty echo </dev/tty 2>/dev/null || true' EXIT INT TERM HUP
    printf "Paste your Weave Router API key (rk_...): "
    stty -echo </dev/tty 2>/dev/null || true
    read -r api_key </dev/tty
    stty echo </dev/tty 2>/dev/null || true
    trap - EXIT INT TERM HUP
    printf "\n"
    [ -n "$api_key" ] || { err "no key provided"; exit 1; }
  fi
fi

# ---------- write the statusline script ----------

cat > "$statusline_file" << 'STATUSLINE_EOF'
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
  ) || true
fi

# Brand color (#FF6C47) on terminals that grok 24-bit truecolor — that's
# every modern one (iTerm2, Apple Terminal, vscode, ghostty, alacritty,
# wezterm, kitty). Falls back gracefully on any escape-stripping terminal.
brand=$'[38;2;255;108;71mWEAVE ROUTER[0m'

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
STATUSLINE_EOF
chmod +x "$statusline_file"
ok "Statusline installed at $statusline_file"

# ---------- patch settings.json ----------

# Build the merge patch. Claude Code keeps its own Anthropic auth in
# Authorization/x-api-key; the router key rides in ANTHROPIC_CUSTOM_HEADERS.
# Project scope (no --dir) writes the key to settings.local.json (gitignored)
# so teammates can share settings.json. --dir and user scope inline the key
# directly into settings.json since there's no team to coordinate with.
tmp_patch="$(mktemp)"
trap 'rm -f "$tmp_patch"' EXIT

if [ "$scope" = "project" ] && [ -z "$install_dir" ]; then
  jq -n --arg url "$base_url" --arg sl "$statusline_path_for_settings" '{
    env: { ANTHROPIC_BASE_URL: $url },
    statusLine: { type: "command", command: $sl }
  }' >"$tmp_patch"
elif [ "$dev_mode" = "true" ]; then
  jq -n --arg url "$base_url" --arg sl "$statusline_path_for_settings" '{
    env: { ANTHROPIC_BASE_URL: $url },
    statusLine: { type: "command", command: $sl }
  }' >"$tmp_patch"
else
  jq -n --arg url "$base_url" --arg header "$router_key_header: $api_key" --arg sl "$statusline_path_for_settings" '{
    env: { ANTHROPIC_BASE_URL: $url, ANTHROPIC_CUSTOM_HEADERS: $header },
    statusLine: { type: "command", command: $sl }
  }' >"$tmp_patch"
fi

# Merge with existing settings. Deep-merge env and replace statusLine.
# We strip router-owned auth from the existing settings BEFORE merging —
# otherwise switching auth mode (key→dev-mode) would leave stale credentials
# behind. ANTHROPIC_AUTH_TOKEN/apiKeyHelper are also removed to migrate older
# installs that used them for router auth.
if [ -f "$settings_file" ]; then
  merged="$(jq -s '.[0] as $a | .[1] as $b
    | $a
    | .env = (($a.env // {} | del(.ANTHROPIC_AUTH_TOKEN, .ANTHROPIC_CUSTOM_HEADERS)) + ($b.env // {}))
    | (if (.env | length) == 0 then del(.env) else . end)
    | del(.apiKeyHelper)
    | (if $b.statusLine then .statusLine = $b.statusLine else . end)
  ' "$settings_file" "$tmp_patch")"
  printf '%s\n' "$merged" >"$settings_file"
else
  cp "$tmp_patch" "$settings_file"
fi
ok "Settings written to $settings_file"

if [ "$scope" = "project" ] && [ -z "$install_dir" ] && [ "$dev_mode" != "true" ]; then
  jq -n --arg header "$router_key_header: $api_key" '{
    env: { ANTHROPIC_CUSTOM_HEADERS: $header }
  }' >"$tmp_patch"
  if [ -f "$local_settings_file" ]; then
    merged="$(jq -s '.[0] as $a | .[1] as $b
      | $a
      | .env = (($a.env // {} | del(.ANTHROPIC_AUTH_TOKEN, .ANTHROPIC_CUSTOM_HEADERS)) + ($b.env // {}))
      | (if (.env | length) == 0 then del(.env) else . end)
      | del(.apiKeyHelper)
    ' "$local_settings_file" "$tmp_patch")"
    printf '%s\n' "$merged" >"$local_settings_file"
  else
    cp "$tmp_patch" "$local_settings_file"
  fi
  chmod 600 "$local_settings_file"
  ok "Router key header written to $local_settings_file"
fi

# ---------- gitignore for project scope ----------

if [ "$scope" = "project" ] && [ -z "$install_dir" ]; then
  gitignore="$git_root/.gitignore"
  # Same symlink containment as the .claude/ paths above: a hostile repo could
  # commit .gitignore as a symlink so the >> below writes outside the repo.
  refuse_if_symlink "$gitignore"
  # Keep the statusline script and per-teammate local settings out of git. The
  # local settings carry the router key header; each teammate gets their own.
  for entry in \
    ".claude/settings.local.json" \
    ".claude/.credentials.json" \
    ".claude/cc-statusline.sh"
  do
    if [ ! -f "$gitignore" ] || ! grep -qxF "$entry" "$gitignore"; then
      printf '%s\n' "$entry" >>"$gitignore"
    fi
  done
  ok "Updated $gitignore (ignored credentials + local helpers)"
fi

# ---------- post-install verification ----------

info "Pinging router at $base_url ..."
if curl -fsS --max-time 5 "$base_url/health" >/dev/null 2>&1; then
  ok "Router is reachable."
else
  warn "Could not reach $base_url/health within 5s. Settings are written; verify the router is running."
fi

if [ "$dev_mode" != "true" ] && [ -n "$api_key" ]; then
  # Pass the router key via stdin (`@-`) instead of a -H argument so the key
  # never appears in the process arg list (visible via `ps` / /proc to other
  # local users on shared machines).
  if printf '%s: %s\n' "$router_key_header" "$api_key" \
      | curl -fsS --max-time 5 --header @- "$base_url/validate" >/dev/null 2>&1; then
    ok "API key validated."
  else
    warn "Router rejected the API key (check it matches the dashboard, or pass --dev-mode for a local ROUTER_DEV_MODE server)."
  fi
fi

# ---------- done ----------

printf "\n"
ok "Weave Router installed for Claude Code."
if [ "$scope" = "project" ]; then
  if [ -n "$install_dir" ]; then
    echo "  Installed into $install_dir/.claude/ (project scope)."
    echo "  Run 'cd $install_dir && claude' to use the router."
    echo "  Uninstall with: $script_dir/uninstall.sh --scope project --dir $install_dir"
  else
    echo "  Commit .claude/settings.json + the .gitignore changes."
    echo "  Local helpers/settings are gitignored — each teammate runs"
    echo "  './router/install/install.sh --scope project' once after cloning."
    if [ "$dev_mode" != "true" ]; then
      echo "  Each teammate also adds this to their shell rc:"
      echo "    export WEAVE_ROUTER_KEY=rk_..."
    fi
    echo "  Uninstall with: $script_dir/uninstall.sh --scope project"
  fi
elif [ -n "$install_dir" ]; then
  echo "  Run 'claude' from $install_dir — the status line will show the routed model + savings."
  echo "  'cd $install_dir && claude' to use the router; run 'claude' elsewhere to skip it."
  echo "  Uninstall with: $script_dir/uninstall.sh --dir $install_dir"
else
  echo "  Run 'claude' anywhere — the status line will show the routed model + savings."
  echo "  Uninstall with: $script_dir/uninstall.sh --scope user"
fi
