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
# Usage:
#   ./install.sh                                  # hosted router, user scope
#   ./install.sh --scope project                  # commit-with-team install
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

# Where to fetch cc-statusline.sh when running via curl|sh and no sibling file
# is present. When running from a clone (sibling file exists), we use that.
STATUSLINE_RAW_URL="${WEAVE_STATUSLINE_RAW_URL:-https://raw.githubusercontent.com/weave-ai/workweave/main/router/install/cc-statusline.sh}"

scope="user"
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

# ---------- pre-flight ----------

info "Weave Router installer (scope=$scope, base_url=$base_url)"

require_cmd jq    "macOS: 'brew install jq' · Debian/Ubuntu: 'sudo apt install jq'"
require_cmd curl  "macOS/Linux: usually preinstalled — check your package manager"

if ! command -v claude >/dev/null 2>&1; then
  warn "'claude' not found on PATH. Install Claude Code from https://claude.com/code, then re-run this script."
  warn "Continuing — settings.json will be written and will take effect once Claude Code is installed."
fi

# Resolve target paths based on scope.
script_dir="$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"
local_statusline="$script_dir/cc-statusline.sh"

case "$scope" in
  user)
    settings_dir="$HOME/.claude"
    settings_file="$settings_dir/settings.json"
    local_settings_file=""
    statusline_dir="$HOME/.weave"
    statusline_file="$statusline_dir/cc-statusline.sh"
    statusline_path_for_settings="$statusline_file"
    ;;
  project)
    if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
      err "--scope project must be run inside a git repo. cd into your project first."
      exit 1
    fi
    settings_dir="$git_root/.claude"
    settings_file="$settings_dir/settings.json"
    local_settings_file="$settings_dir/settings.local.json"
    statusline_dir="$git_root/.claude"
    statusline_file="$statusline_dir/cc-statusline.sh"
    # Use a path that's portable across teammates' machines (relative to repo root).
    statusline_path_for_settings="\${CLAUDE_PROJECT_DIR}/.claude/cc-statusline.sh"
    ;;
esac

# Symlink containment: refuse if any target path (or its parent dir for project
# scope) is a symlink. User-scope paths under $HOME are trusted; project-scope
# paths come from a git repo that may be hostile, so we check those.
if [ "$scope" = "project" ]; then
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

if [ -f "$local_statusline" ]; then
  cp "$local_statusline" "$statusline_file"
  ok "Statusline script copied from $local_statusline"
else
  info "Fetching statusline script from $STATUSLINE_RAW_URL"
  curl -fsSL "$STATUSLINE_RAW_URL" -o "$statusline_file" \
    || { err "failed to download cc-statusline.sh — re-run from a clone or set WEAVE_STATUSLINE_RAW_URL"; exit 1; }
fi
chmod +x "$statusline_file"
ok "Statusline installed at $statusline_file"

# ---------- patch settings.json ----------

# Build the merge patch. Claude Code keeps its own Anthropic auth in
# Authorization/x-api-key; the router key rides in ANTHROPIC_CUSTOM_HEADERS.
# Project scope writes the key to settings.local.json (gitignored).
tmp_patch="$(mktemp)"
trap 'rm -f "$tmp_patch"' EXIT

if [ "$scope" = "user" ]; then
  if [ "$dev_mode" = "true" ]; then
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
else
  # project scope
  jq -n --arg url "$base_url" --arg sl "$statusline_path_for_settings" '{
    env: { ANTHROPIC_BASE_URL: $url },
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

if [ "$scope" = "project" ] && [ "$dev_mode" != "true" ]; then
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

if [ "$scope" = "project" ]; then
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
case "$scope" in
  user)
    echo "  Run 'claude' anywhere — the status line will show the routed model + savings."
    ;;
  project)
    echo "  Commit .claude/settings.json + the .gitignore changes."
    echo "  Local helpers/settings are gitignored — each teammate runs"
    echo "  './router/install/install.sh --scope project' once after cloning."
    if [ "$dev_mode" != "true" ]; then
      echo "  Each teammate also adds this to their shell rc:"
      echo "    export WEAVE_ROUTER_KEY=rk_..."
    fi
    ;;
esac
echo "  Uninstall with: $script_dir/uninstall.sh${scope:+ --scope $scope}"
