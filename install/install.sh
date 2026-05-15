#!/usr/bin/env bash
#
# Weave Router installer for Claude Code and Codex.
#
# Configures Claude Code (default) or the OpenAI Codex CLI (`--codex`) to
# permanently route through the Weave Router. For Claude Code this writes
# the router base URL, router auth header, and a status line into Claude
# Code's settings.json. For Codex it writes a `model_providers.weave`
# entry plus `model_provider = "weave"` into ~/.codex/config.toml (managed
# block delimited by markers so re-install / uninstall is clean).
#
# Two scopes (apply to both targets):
#   - user (default):  ~/.claude/settings.json  + ~/.weave/cc-statusline.sh
#                      ~/.codex/config.toml                       (with --codex)
#   - project:         <repo>/.claude/settings.json + <repo>/.claude/cc-statusline.sh
#                      <repo>/.codex/config.toml                  (with --codex)
#
# Or pass --dir to install into any directory:
#   - dir:              <dir>/.claude/settings.json + <dir>/.claude/cc-statusline.sh
#                       <dir>/.codex/config.toml                  (with --codex)
#
# Usage:
#   npx @workweave/router                                  # interactive picker (Claude Code or Codex)
#   npx @workweave/router --claude                         # skip the picker, target Claude Code
#   npx @workweave/router --codex                          # skip the picker, target the OpenAI Codex CLI
#   npx @workweave/router --scope project                  # commit-with-team install
#   npx @workweave/router --dir /tmp/my-sandbox            # isolated throwaway install
#   npx @workweave/router --local                          # local router on localhost:8080
#   npx @workweave/router --base-url http://localhost:8080 # self-hosted, custom port
#   npx @workweave/router --non-interactive                # require WEAVE_ROUTER_KEY env var (defaults target to claude)
#   npx @workweave/router --quiet                          # suppress banner, ping check, and trailing tips
#   npx @workweave/router --uninstall                      # remove a previous install (delegates to uninstall.sh)

set -euo pipefail

# ---------- defaults ----------

# The hosted Weave Router URL. Override with --base-url for self-hosted.
DEFAULT_BASE_URL="${WEAVE_ROUTER_URL:-https://router.workweave.ai}"


scope="user"
scope_explicit="false"
install_dir=""
base_url=""
non_interactive="false"
quiet="false"
router_key_header="X-Weave-Router-Key"
# Target tool whose config we patch. "claude" (default) writes Claude Code
# settings.json; "codex" writes ~/.codex/config.toml. Each target carries its
# own credential-passthrough story in the router: Claude Code's logged-in
# Anthropic key flows through unchanged, and Codex's `OPENAI_API_KEY` flows
# through via the same header path. target_explicit tracks whether --claude
# or --codex was passed so an interactive run can prompt for the choice.
target="claude"
target_explicit="false"

# ---------- helpers ----------

# Detect whether stdout is a real terminal that grokks ANSI escapes. Pipes,
# CI logs, and `curl ... | sh` redirects all fail this check, so we degrade
# to plain ASCII output instead of leaking raw escape bytes.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  tty_out="true"
else
  tty_out="false"
fi

# Brand color (#FF6C47) plus a few supporting shades. Truecolor escapes work
# on every modern terminal (iTerm2, Apple Terminal, vscode, ghostty, alacritty,
# wezterm, kitty); on TTY-less output we blank them out.
if [ "$tty_out" = "true" ]; then
  C_BRAND=$'\033[38;2;255;108;71m'
  C_DIM=$'\033[2m'
  C_BOLD=$'\033[1m'
  C_RED=$'\033[31m'
  C_YELLOW=$'\033[33m'
  C_GREEN=$'\033[32m'
  C_CYAN=$'\033[36m'
  C_RESET=$'\033[0m'
else
  C_BRAND=""; C_DIM=""; C_BOLD=""; C_RED=""; C_YELLOW=""; C_GREEN=""; C_CYAN=""; C_RESET=""
fi

err()  { printf "%serror:%s %s\n" "$C_RED" "$C_RESET" "$*" >&2; }
warn() { printf "%swarning:%s %s\n" "$C_YELLOW" "$C_RESET" "$*" >&2; }
info() { printf "%s==>%s %s\n" "$C_CYAN" "$C_RESET" "$*"; }
ok()   { printf "%s✓%s %s\n" "$C_GREEN" "$C_RESET" "$*"; }
skip() { printf "%s⊙%s %s%s%s\n" "$C_DIM" "$C_RESET" "$C_DIM" "$*" "$C_RESET"; }

# ---------- banner ----------
#
# Print the WEAVE wordmark in brand orange. Skipped under --quiet or when
# stdout isn't a TTY so log captures don't get junk box-drawing chars.
print_banner() {
  [ "$quiet" = "true" ] && return 0
  [ "$tty_out" = "true" ] || return 0
  local target_label
  case "$target" in
    codex) target_label="Codex installer" ;;
    *)     target_label="Claude Code installer" ;;
  esac
  printf '\n'
  printf '%s  ╦ ╦╔═╗╔═╗╦  ╦╔═╗%s\n' "$C_BRAND" "$C_RESET"
  printf '%s  ║║║║╣ ╠═╣╚╗╔╝║╣ %s\n' "$C_BRAND" "$C_RESET"
  printf '%s  ╚╩╝╚═╝╩ ╩ ╚╝ ╚═╝%s\n' "$C_BRAND" "$C_RESET"
  printf '  %sWeave Router · %s%s\n\n' "$C_DIM" "$target_label" "$C_RESET"
}

# ---------- spinner ----------
#
# Pure-bash spinner. `spin "label" cmd args...` runs cmd in the background,
# cycles dots frames in place while it runs, then replaces the line with
# ✓ or ✗ depending on exit status. Skipped (synchronous fallback) when
# stdout is not a TTY — pipes and CI logs would otherwise eat the carriage
# returns and leave a blob of frames. The command's own stdout/stderr is
# captured to $spin_log so we can echo it on failure for debugging.
#
# Frame set is `dots` from sindresorhus/cli-spinners.
SPIN_FRAMES='⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏'
SPIN_INTERVAL=0.08
spin_pid=""
spin_log=""

_spin_cleanup() {
  # Kill any active spinner child and restore the cursor. Called from the
  # global EXIT/INT/TERM/HUP trap so Ctrl-C never leaves a dangling spinner
  # process or a hidden cursor behind.
  if [ -n "$spin_pid" ] && kill -0 "$spin_pid" 2>/dev/null; then
    kill "$spin_pid" 2>/dev/null || true
    wait "$spin_pid" 2>/dev/null || true
  fi
  spin_pid=""
  if [ "$tty_out" = "true" ]; then
    printf '\033[?25h' # show cursor
  fi
  [ -n "$spin_log" ] && rm -f "$spin_log" 2>/dev/null || true
  # Also restore stty echo in case we died mid-keypaste prompt. macOS
  # `[ -r /dev/tty ]` returns true even when the underlying device errors
  # on open (ENXIO "Device not configured") under `curl | sh` and CI, so
  # we gate on stdin being an actual tty before touching it.
  if [ -t 0 ]; then
    stty echo 2>/dev/null || true
  fi
}
trap _spin_cleanup EXIT INT TERM HUP

spin() {
  local label="$1"; shift
  if [ "$tty_out" != "true" ] || [ "$quiet" = "true" ]; then
    # No spinner — just run the command and emit a single check line after.
    if "$@" >/dev/null 2>&1; then
      ok "$label"
      return 0
    else
      local rc=$?
      printf "%s✗%s %s\n" "$C_RED" "$C_RESET" "$label" >&2
      return $rc
    fi
  fi

  spin_log="$(mktemp -t weave-install.XXXXXX)"
  ( "$@" >"$spin_log" 2>&1 ) &
  spin_pid=$!

  printf '\033[?25l' # hide cursor
  local i=0
  # shellcheck disable=SC2206
  local frames=($SPIN_FRAMES)
  local n=${#frames[@]}
  while kill -0 "$spin_pid" 2>/dev/null; do
    printf '\r%s%s%s %s' "$C_BRAND" "${frames[i]}" "$C_RESET" "$label"
    i=$(( (i + 1) % n ))
    sleep "$SPIN_INTERVAL"
  done

  wait "$spin_pid"
  local rc=$?
  spin_pid=""
  printf '\033[?25h' # show cursor
  printf '\r\033[2K' # clear line

  if [ $rc -eq 0 ]; then
    printf '%s✓%s %s\n' "$C_GREEN" "$C_RESET" "$label"
    rm -f "$spin_log"
    spin_log=""
    return 0
  else
    printf '%s✗%s %s\n' "$C_RED" "$C_RESET" "$label" >&2
    if [ -s "$spin_log" ]; then
      printf '%s' "$C_DIM" >&2
      sed 's/^/    /' "$spin_log" >&2
      printf '%s' "$C_RESET" >&2
    fi
    rm -f "$spin_log"
    spin_log=""
    return $rc
  fi
}

usage() {
  # Print the leading comment block (lines 2..just-before `set -euo`), stripping
  # the leading `# `. awk avoids GNU `head -n -<N>`, which BSD head on macOS
  # rejects with "illegal line count -- -N". Banner sits above so `--help`
  # gets the same wordmark as a fresh install run.
  print_banner
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

# Markers that delimit the block this installer manages inside Codex's
# config.toml. Kept on disk verbatim so a re-install (or uninstall.sh
# --codex) can find and replace the block instead of duplicating it.
WEAVE_CODEX_BEGIN_MARKER="# >>> weave-router managed (do not edit between markers) >>>"
WEAVE_CODEX_END_MARKER="# <<< weave-router managed <<<"

# write_codex_config writes a managed [model_providers.weave] block to the
# Codex CLI's config.toml. Sets `model_provider = "weave"` at the top level so
# Codex picks the routed provider by default. Both lines live inside the
# managed-block markers so uninstall removes them cleanly. We strip any
# top-level `model_provider = ...` declaration OUTSIDE the markers before
# appending so the file doesn't end up with a duplicate key (TOML rejects
# that). Inline `model_provider` keys inside `[profiles.*]` sections stay
# untouched.
#
# Usage: write_codex_config <config_file_path> <base_url> <api_key>
write_codex_config() {
  local config_file="$1"
  local block_url="$2"
  local block_key="$3"

  local block
  block="$(cat <<TOML
${WEAVE_CODEX_BEGIN_MARKER}
# Managed by the Weave Router installer. Re-running the installer rewrites
# this block; \`./uninstall.sh --codex\` removes it. To opt out without
# uninstalling, change the model_provider value below.
model_provider = "weave"

[model_providers.weave]
name = "Weave Router"
base_url = "${block_url}/v1"
env_key = "OPENAI_API_KEY"
wire_api = "chat"
http_headers = { "X-Weave-Router-Key" = "${block_key}" }
${WEAVE_CODEX_END_MARKER}
TOML
)"

  if [ -f "$config_file" ]; then
    local tmp; tmp="$(mktemp -t weave-codex.XXXXXX)"
    # Strip the managed block (between markers) plus any top-level
    # `model_provider =` outside it. We define "top-level" as everything
    # before the first `[section]` header. The awk handles both passes in
    # one sweep so we never emit a duplicate.
    awk -v begin="$WEAVE_CODEX_BEGIN_MARKER" -v end="$WEAVE_CODEX_END_MARKER" '
      $0 == begin { skip = 1; next }
      $0 == end   { skip = 0; next }
      skip        { next }
      /^[[:space:]]*\[/ { in_section = 1 }
      !in_section && /^[[:space:]]*model_provider[[:space:]]*=/ { next }
      { print }
    ' "$config_file" >"$tmp"
    mv "$tmp" "$config_file"
    # Leading newline keeps the appended block readable when the file ended
    # without one (mktemp output may or may not have trailing newline).
    printf "\n%s\n" "$block" >>"$config_file"
  else
    printf "%s\n" "$block" >"$config_file"
  fi
  # 0600: the file holds a router key. Even at user scope, mode 644 would
  # leak the key to any local user on a shared box.
  chmod 600 "$config_file"
}

# ---------- uninstall delegation ----------
#
# `--uninstall` flips this script into a thin shim for uninstall.sh: the
# canonical uninstall logic lives in a sibling file, and we want both
# direct invocations (`./install.sh --uninstall`) and curl-piped ones
# (`curl ... | sh -s -- --uninstall`) to behave the same as
# `npx @workweave/router --uninstall` (which bin.js routes to uninstall.sh on
# its own).
#
# Scan every arg, not just $1, so flag order doesn't matter; build a clean
# list with --uninstall stripped and exec uninstall.sh with the remainder.
#
# Resolution order for the uninstall script:
#   1. Sibling file next to install.sh on disk (npm tarball / git checkout).
#   2. WEAVE_UNINSTALL_URL override (self-hosters who fork).
#   3. Default: raw.githubusercontent.com canonical copy (curl|sh path).
for arg in "$@"; do
  if [ "$arg" = "--uninstall" ]; then
    cleaned_args=()
    for a in "$@"; do
      [ "$a" = "--uninstall" ] || cleaned_args+=("$a")
    done

    script_path="${BASH_SOURCE[0]:-$0}"
    if [ -f "$script_path" ]; then
      sibling_dir="$(cd "$(dirname "$script_path")" 2>/dev/null && pwd)"
      if [ -n "$sibling_dir" ] && [ -f "$sibling_dir/uninstall.sh" ]; then
        exec bash "$sibling_dir/uninstall.sh" "${cleaned_args[@]+"${cleaned_args[@]}"}"
      fi
    fi

    require_cmd curl "https://curl.se"
    url="${WEAVE_UNINSTALL_URL:-https://raw.githubusercontent.com/workweave/router/main/install/uninstall.sh}"
    # Pull the body into memory and exec via `bash -c` so we never touch
    # disk: `exec` replaces this process, so any temp file we wrote would
    # outlive the EXIT trap and leak indefinitely. Loading into a variable
    # also gives us a chance to fail closed on 404 HTML pages before
    # handing the content to bash.
    if ! uninstall_body="$(curl -fsSL --max-time 30 "$url" 2>/dev/null)"; then
      err "failed to fetch uninstall.sh from $url"
      exit 1
    fi
    if [ -z "$uninstall_body" ] || [ "${uninstall_body:0:2}" != "#!" ]; then
      err "fetched content from $url doesn't look like a bash script"
      exit 1
    fi
    exec bash -c "$uninstall_body" weave-uninstall "${cleaned_args[@]+"${cleaned_args[@]}"}"
  fi
done

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
    --local)
      # Shorthand for local dev: localhost:8080 (matches `wv mr` / `make dev` default PORT).
      base_url="http://localhost:8080"
      shift
      ;;
    --non-interactive)
      non_interactive="true"; shift
      ;;
    --quiet)
      quiet="true"; shift
      ;;
    --dir)
      install_dir="${2:-}"; shift 2
      [ -n "$install_dir" ] || { err "--dir requires a path"; exit 2; }
      ;;
    --codex)
      target="codex"; target_explicit="true"; shift
      ;;
    --claude)
      # No-op selector for symmetry with --codex. Useful in pipelines that
      # want to skip the interactive picker without depending on the default.
      target="claude"; target_explicit="true"; shift
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

# ---------- interactive target prompt ----------

# If neither --claude nor --codex was passed and we have a controlling
# terminal, ask which tool to install for. Non-interactive runs (CI,
# `curl | sh --non-interactive`) silently use the "claude" default — same
# behavior the script had before --codex existed, so existing pipelines
# don't change semantics. We prompt BEFORE print_banner so the banner's
# target label (Claude Code installer / Codex installer) reflects the choice.
if [ "$target_explicit" = "false" ] && [ "$non_interactive" = "false" ] && [ -r /dev/tty ]; then
  printf "%sInstall target:%s\n" "$C_BOLD" "$C_RESET"
  printf "  %s1)%s Claude Code  %s— patches ~/.claude/settings.json (or <repo>/.claude/)%s\n" "$C_BRAND" "$C_RESET" "$C_DIM" "$C_RESET"
  printf "  %s2)%s Codex        %s— patches ~/.codex/config.toml (or <repo>/.codex/)%s\n" "$C_BRAND" "$C_RESET" "$C_DIM" "$C_RESET"
  printf "Choose %s[1/2]%s (default %s1%s): " "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET"
  read -r target_choice </dev/tty || target_choice=""
  case "${target_choice:-1}" in
    1|""|claude|c|C)  target="claude" ;;
    2|codex|x|X)      target="codex" ;;
    *) err "invalid choice: $target_choice"; exit 2 ;;
  esac
fi

# Banner runs before the interactive scope prompt so the very first thing
# users see when `make full-setup` hands off to install.sh is the wordmark,
# not a bare "Install scope:" line. Target prompt above already finalized
# $target, so the banner's per-target label reflects the user's choice.
print_banner

# ---------- interactive scope prompt ----------

# If the user didn't pass --scope and we have a controlling terminal, ask which
# scope to install into. Non-interactive runs (CI, `curl | sh --non-interactive`)
# silently use the "user" default.
if [ -z "$install_dir" ] && [ "$scope_explicit" = "false" ] && [ "$non_interactive" = "false" ] && [ -r /dev/tty ]; then
  printf "%sInstall scope:%s\n" "$C_BOLD" "$C_RESET"
  printf "  %s1)%s user     %s— write to ~/.claude/ (applies everywhere you run claude)%s\n" "$C_BRAND" "$C_RESET" "$C_DIM" "$C_RESET"
  printf "  %s2)%s project  %s— write to <repo>/.claude/ (applies only inside this repo)%s\n" "$C_BRAND" "$C_RESET" "$C_DIM" "$C_RESET"
  printf "Choose %s[1/2]%s (default %s1%s): " "$C_BOLD" "$C_RESET" "$C_BOLD" "$C_RESET"
  read -r scope_choice </dev/tty || scope_choice=""
  case "${scope_choice:-1}" in
    1|""|user|u|U)    scope="user" ;;
    2|project|p|P)    scope="project" ;;
    *) err "invalid choice: $scope_choice"; exit 2 ;;
  esac

  # For project scope, ask which directory rather than silently assuming CWD.
  # A user running this from a shell that happens to be in $HOME or some
  # unrelated repo would otherwise scribble .claude/ into the wrong place.
  if [ "$scope" = "project" ]; then
    default_project_dir="$(pwd)"
    printf "Project directory [default: %s]: " "$default_project_dir"
    read -r project_dir_choice </dev/tty || project_dir_choice=""
    project_dir="${project_dir_choice:-$default_project_dir}"
    # Expand a leading ~ since `read` doesn't.
    case "$project_dir" in
      "~")    project_dir="$HOME" ;;
      "~/"*)  project_dir="$HOME/${project_dir#~/}" ;;
    esac
    if [ ! -d "$project_dir" ]; then
      err "directory does not exist: $project_dir"
      exit 1
    fi
    project_dir="$(cd "$project_dir" && pwd)"
  fi
fi

# ---------- pre-flight ----------

[ "$quiet" = "true" ] || info "scope=${C_BOLD}${scope}${C_RESET}  target=${C_BOLD}${target}${C_RESET}  base_url=${C_BOLD}${base_url}${C_RESET}"

# Codex install only writes a TOML file (managed via awk) so jq isn't needed.
# Claude Code's settings.json patching uses jq to deep-merge env keys.
if [ "$target" = "claude" ]; then
  require_cmd jq    "macOS: 'brew install jq' · Debian/Ubuntu: 'sudo apt install jq'"
fi
require_cmd curl  "macOS/Linux: usually preinstalled — check your package manager"

case "$target" in
  claude)
    if ! command -v claude >/dev/null 2>&1; then
      warn "'claude' not found on PATH. Install Claude Code from https://claude.com/code, then re-run this script."
      warn "Continuing — settings.json will be written and will take effect once Claude Code is installed."
    fi
    ;;
  codex)
    if ! command -v codex >/dev/null 2>&1; then
      warn "'codex' not found on PATH. Install via 'npm install -g @openai/codex' (or brew install codex), then re-run this script."
      warn "Continuing — config.toml will be written and will take effect once Codex is installed."
    fi
    ;;
esac

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
      # If the interactive prompt collected a project directory, use it.
      # Otherwise fall back to the git root of CWD (the original behavior,
      # preserved for --scope project passed on the command line).
      if [ -n "${project_dir:-}" ]; then
        settings_base="$project_dir"
        git_root="$(cd "$project_dir" && git rev-parse --show-toplevel 2>/dev/null || true)"
      else
        if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
          err "--scope project must be run inside a git repo, or pass --dir <path>. cd into your project first, or use --dir."
          exit 1
        fi
        settings_base="$git_root"
      fi
      ;;
  esac
fi

if [ "$target" = "claude" ]; then
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

  # Symlink containment: refuse if any target path is a symlink. User-scope
  # paths under $HOME are trusted; project-scope and --dir paths come from a
  # git repo or user-supplied directory that may be hostile, so we check those.
  if [ "$scope" = "project" ] || [ -n "$install_dir" ]; then
    refuse_if_symlink "$settings_dir"
    refuse_if_symlink "$settings_file"
    refuse_if_symlink "$local_settings_file"
    refuse_if_symlink "$statusline_file"
  fi

  mkdir -p "$settings_dir" "$statusline_dir"
else
  # Codex CLI reads config from ~/.codex/config.toml by default. For project
  # scope we write to <repo>/.codex/config.toml; the user invokes Codex with
  # `CODEX_HOME=<repo>/.codex codex` (or runs from the repo if Codex auto-
  # discovers). The router key is embedded in the file so it stays per-
  # teammate — .codex/config.toml goes in .gitignore in project scope.
  codex_dir="$settings_base/.codex"
  codex_config_file="$codex_dir/config.toml"

  if [ "$scope" = "project" ] || [ -n "$install_dir" ]; then
    refuse_if_symlink "$codex_dir"
    refuse_if_symlink "$codex_config_file"
  fi

  mkdir -p "$codex_dir"
fi

# ---------- token handling ----------

api_key=""
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
    # _spin_cleanup (installed globally above) already restores stty echo on
    # any exit path, so we don't need a separate trap here — that would
    # overwrite the spinner cleanup and leak the cursor / child PID on Ctrl-C.
    printf "%sPaste your Weave Router API key (rk_...):%s " "$C_DIM" "$C_RESET"
    stty -echo </dev/tty 2>/dev/null || true
    read -r api_key </dev/tty
    stty echo </dev/tty 2>/dev/null || true
    printf "\n"
    [ -n "$api_key" ] || { err "no key provided"; exit 1; }
  fi

# ---------- codex install path (dispatch + exit before the Claude-only writes) ----------

if [ "$target" = "codex" ]; then
  write_codex_config "$codex_config_file" "$base_url" "$api_key"
  ok "Codex config written to $codex_config_file"

  # Project scope: ensure the per-teammate config (which holds the router key)
  # is gitignored. The base URL is the same for every teammate, so a
  # committed shared file would still leak the per-key portion. Easier to
  # ignore the whole config and have each teammate run the installer.
  if [ "$scope" = "project" ] && [ -z "$install_dir" ] && [ -n "${git_root:-}" ]; then
    gitignore="$git_root/.gitignore"
    refuse_if_symlink "$gitignore"
    for entry in \
      ".codex/config.toml"
    do
      if [ ! -f "$gitignore" ] || ! grep -qxF "$entry" "$gitignore"; then
        printf '%s\n' "$entry" >>"$gitignore"
      fi
    done
    ok "Updated $gitignore (ignored .codex/config.toml)"
  fi

  # Post-install verification: same probes the Claude path runs so a working
  # install gives the same green checks regardless of target.
  if [ "$quiet" != "true" ]; then
    if ! spin "Pinging $base_url/health" curl -fsS --max-time 5 "$base_url/health"; then
      warn "Could not reach $base_url/health within 5s. Settings are written; verify the router is running."
    fi
  fi

  if [ -n "$api_key" ]; then
    # Pass the router key via stdin (`@-`) instead of -H so it never lands in
    # the process arg list. Mirrors the Claude-path validate logic.
    validate_codex_key() {
      printf '%s: %s\n' "$router_key_header" "$api_key" \
        | curl -fsS --max-time 5 --header @- "$base_url/validate"
    }
    if ! spin "Validating API key" validate_codex_key; then
      warn "Router rejected the API key (check it matches the dashboard at $base_url/ui/)."
    fi
  fi

  printf "\n"
  printf "%s✓%s %s%sWeave Router installed for Codex.%s\n" \
    "$C_GREEN" "$C_RESET" "$C_BOLD" "$C_BRAND" "$C_RESET"
  if [ "$scope" = "project" ] || [ -n "$install_dir" ]; then
    # Codex auto-discovers ~/.codex; for non-user installs the caller has to
    # point CODEX_HOME at the directory we wrote so Codex finds our config.
    info "Run Codex with CODEX_HOME=$codex_dir codex so it picks up this config."
  fi
  exit 0
fi

# ---------- write the statusline script ----------

cat > "$statusline_file" << 'STATUSLINE_EOF'
#!/usr/bin/env bash
#
# Claude Code statusline for the Weave router. CC pipes a JSON blob on stdin
# whose `transcript_path` points at the JSONL log of the current session and
# whose `model.display_name` is the user's CC-side model selection. The
# router rewrites each request's `model` field before forwarding, so
# Anthropic/OpenAI/Google return `message.model = <routed>` in the SSE
# stream and CC stores that in the transcript verbatim. Per-turn savings
# come from comparing each turn's routed cost against what the user's
# selection would have cost on the same tokens. Works identically for
# local docker and the managed cloud router — no sidecar, no DB, no auth.
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

# ---------- background self-refresh ----------
#
# Once every WEAVE_STATUSLINE_UPDATE_INTERVAL_DAYS (default 7), check
# raw.githubusercontent.com for a newer copy of this script and swap it in
# atomically. Runs in a forked subshell so the current Claude turn never
# blocks; the next turn picks up the new version. Applies to both user-scope
# (~/.weave/cc-statusline.sh) and project-scope (<repo>/.claude/cc-statusline.sh)
# installs — project teammates rate-limit independently because the stamp
# lives in their per-user cache dir, and on no-content-change days we skip
# the mv entirely so the repo working tree stays clean. When upstream does
# change, the first teammate's commit propagates the new version to the rest.
#
# Opt out entirely with `export WEAVE_STATUSLINE_UPDATE=0`. Override the
# source with `WEAVE_STATUSLINE_URL=...`, e.g. for self-hosters who fork.
weave_self_refresh() {
  [ "${WEAVE_STATUSLINE_UPDATE:-1}" = "0" ] && return 0
  command -v curl >/dev/null 2>&1 || return 0

  local self="${BASH_SOURCE[0]:-$0}"
  [ -f "$self" ] && [ -w "$self" ] || return 0

  local interval_days="${WEAVE_STATUSLINE_UPDATE_INTERVAL_DAYS:-7}"
  local interval_seconds=$(( interval_days * 86400 ))

  # Stamp lives in the per-user cache dir, keyed by absolute script path so
  # multiple repos (and the user-scope copy) rate-limit independently and no
  # stray file ever lands inside a repo working tree.
  local cache_dir="${XDG_CACHE_HOME:-$HOME/.cache}/weave-router"
  mkdir -p "$cache_dir" 2>/dev/null || return 0
  local script_slug
  script_slug="$(printf '%s' "$self" | tr -c 'A-Za-z0-9._-' '_')"
  local stamp="$cache_dir/checked-at${script_slug}"

  local now stamp_mtime
  now="$(date +%s 2>/dev/null)" || return 0
  if [ -f "$stamp" ]; then
    # Try GNU `stat -c %Y` first; on macOS (BSD stat) -c isn't recognized
    # and exits non-zero, so we fall through to `stat -f %m`. The reverse
    # order is broken: GNU `stat -f` is `--file-system`, which silently
    # succeeds with multi-line filesystem info instead of failing, leaving
    # $stamp_mtime as garbage and disabling the rate-limit check entirely.
    stamp_mtime="$(stat -c %Y "$stamp" 2>/dev/null || stat -f %m "$stamp" 2>/dev/null)" || stamp_mtime=0
  else
    stamp_mtime=0
  fi
  if [ -n "${stamp_mtime:-}" ] && [ "$stamp_mtime" -gt 0 ] \
     && [ $(( now - stamp_mtime )) -lt "$interval_seconds" ]; then
    return 0
  fi

  # Touch the stamp BEFORE forking so concurrent statusline invocations
  # (Claude calls us on every turn) don't all kick off downloads.
  : > "$stamp" 2>/dev/null || return 0

  local url="${WEAVE_STATUSLINE_URL:-https://raw.githubusercontent.com/workweave/router/main/install/cc-statusline.sh}"
  local tmp="${self}.tmp.$$"
  (
    # Detach stdin (CC pipes JSON to us) so curl can't accidentally consume
    # it, and silence all output so nothing leaks into the statusline.
    exec </dev/null
    if curl -fsSL --max-time 15 "$url" -o "$tmp" 2>/dev/null \
       && [ -s "$tmp" ] \
       && head -n 1 "$tmp" | grep -q '^#!.*bash' \
       && [ "$(wc -c < "$tmp")" -ge 1024 ]; then
      # No-op when the download matches what's already on disk — keeps git
      # status clean for project-scope teammates during a routine refresh.
      if cmp -s "$tmp" "$self"; then
        rm -f "$tmp"
      else
        chmod +x "$tmp" 2>/dev/null || true
        mv "$tmp" "$self" 2>/dev/null || rm -f "$tmp"
      fi
    else
      rm -f "$tmp"
    fi
  ) >/dev/null 2>&1 &
  disown 2>/dev/null || true
  return 0
}
weave_self_refresh 2>/dev/null || true

input="$(cat)"
transcript_path="$(printf '%s' "$input" | jq -r '.transcript_path // empty')"
# Prefer model.id over display_name: pricing keys + the routed model id in
# the transcript are canonical ids (e.g. claude-opus-4-7), while display_name
# is a human label ("Opus 4.7 (1M context)") that won't hit the pricing table,
# zeroing out savings. id passes through normalize_model cleanly.
selected_display="$(printf '%s' "$input" | jq -r '.model.id // .model.display_name // "?"')"

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
    "claude-opus-4-6":                  0.015,
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
    "claude-opus-4-6":                  0.075,
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
tot_cache_read=0
tot_cache_write=0

# Per-turn savings compare each turn's routed cost (priced from
# message.model in the transcript) against what the CC-side model selection
# (selected_display) would have cost on the same tokens. The selection
# isn't strictly the per-turn "requested" model — CC tags some background
# side-calls (compaction probes, title-gen) with a different model id —
# but for those the planner short-circuits to a hard pin and the savings
# math zeroes out anyway. Turns where routed == selection or where either
# model isn't in the pricing table emit 0 savings; the tokens clause
# always renders.

# Normalize the CC-side selection once for use in the jq math below.
requested_norm="$(normalize_model "$selected_display")"

if [[ -n "$transcript_path" && -f "$transcript_path" ]]; then
  # macOS ships `tail -r`, GNU coreutils ships `tac`. Either works to walk the
  # JSONL in reverse so we can grab the latest assistant turn.
  if command -v tac >/dev/null 2>&1; then reverse=(tac); else reverse=(tail -r); fi

  # CC stamps message.model = "<synthetic>" on assistant turns it generated
  # locally (errored requests, cancellations, tool-only stubs) instead of a
  # real model id. Show that as "failure" rather than leaking the internal
  # sentinel into the statusline.
  routed="$("${reverse[@]}" "$transcript_path" 2>/dev/null \
    | jq -r 'select(.type=="assistant") | .message.model // empty' \
    | head -n 1 || true)"
  if [[ "$routed" == "<synthetic>" ]]; then
    routed="failure"
  else
    routed="$(normalize_model "$routed")"
  fi

  # Compute a session running total: savings across every assistant turn
  # whose marker reports a requested ≠ routed swap, plus cumulative token
  # counts across every assistant turn (rerouted or not — total work the
  # session has done). cache_creation is priced at 1.25× input, cache_read
  # at 0.1× — both ratios are stable across the Claude family and a no-op
  # when the provider doesn't return those fields. Cache reads ARE included
  # in the savings comparison: both costs apply the same 0.1× weight to
  # cache_read_input_tokens, so the delta reflects the model-price
  # difference on the cached portion as well.
  #
  # The marker regex tolerates the optional "(<provider>)" segment and a
  # `[1m]` / `-YYYYMMDD` suffix on either model name so transcripts written
  # against context-tiered or dated model ids still parse cleanly.
  read -r session_savings tot_in tot_out tot_cache_read tot_cache_write < <(
    jq -r --argjson p "$prices" --arg requested "$requested_norm" '
      select(.type=="assistant") |
      .message as $m |
      ($m.model // "" | sub("\\[[^]]*\\]$"; "") | sub("-[0-9]{8}$"; "")) as $rm |
      {
        in:    ($m.usage.input_tokens // 0),
        out:   ($m.usage.output_tokens // 0),
        cwrt:  ($m.usage.cache_creation_input_tokens // 0),
        crd:   ($m.usage.cache_read_input_tokens // 0)
      } as $t |
      (if $requested == "" or $requested == $rm then 0
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
       end) as $savings |
      "\($savings) \($t.in) \($t.out) \($t.crd) \($t.cwrt)"
    ' "$transcript_path" 2>/dev/null \
    | awk 'BEGIN{s=0; i=0; o=0; r=0; w=0}
           {s+=$1; i+=$2; o+=$3; r+=$4; w+=$5}
           END{printf "%.4f %d %d %d %d\n", s, i, o, r, w}'
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

# cache_read tokens are the cached portion of every prompt that the
# provider serves at 0.1× input price; cache_write tokens are the bytes
# that get newly cached on this turn at 1.25× input price. They behave
# completely differently both in cost and in what they tell the user
# about session-level efficiency, so we surface them separately rather
# than summing into a single "cached" number that conflates the two.
# Each clause is shown only when nonzero, so quiet sessions stay quiet.
tokens_clause=""
if [[ "$tot_in" -gt 0 || "$tot_out" -gt 0 || "$tot_cache_read" -gt 0 || "$tot_cache_write" -gt 0 ]]; then
  tokens_clause=" · $(fmt_tok "$tot_in") in / $(fmt_tok "$tot_out") out"
  if [[ "$tot_cache_read" -gt 0 ]]; then
    tokens_clause+=" / $(fmt_tok "$tot_cache_read") cache read"
  fi
  if [[ "$tot_cache_write" -gt 0 ]]; then
    tokens_clause+=" / $(fmt_tok "$tot_cache_write") cache write"
  fi
fi

if [[ "$routed" == "failure" ]]; then
  # Latest turn was a CC-synthesized error stub — don't claim a routing
  # swap or compute savings against a non-model.
  printf '%s — %s%s' "$brand" "$routed" "$tokens_clause"
elif [[ -n "$routed" ]]; then
  # Show the savings clause only when the session is genuinely net-saving.
  # session_savings is "0.0000" on fresh sessions or sessions where every
  # turn routed back to the selected model; it can also go negative when
  # sticky routing forces a haiku-tagged side-call up to a cached
  # sonnet/opus decision. In both cases the word "saved" would mislead,
  # so drop the savings clause but keep the token totals.
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
# Compose with the spinner cleanup trap installed above — replacing it would
# leave the cursor hidden if Ctrl-C lands during settings.json patching.
trap '_spin_cleanup; rm -f "$tmp_patch"' EXIT INT TERM HUP

if [ "$scope" = "project" ] && [ -z "$install_dir" ]; then
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

if [ "$scope" = "project" ] && [ -z "$install_dir" ]; then
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

if [ "$scope" = "project" ] && [ -z "$install_dir" ] && [ -n "${git_root:-}" ]; then
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

if [ "$quiet" != "true" ]; then
  if ! spin "Pinging $base_url/health" curl -fsS --max-time 5 "$base_url/health"; then
    warn "Could not reach $base_url/health within 5s. Settings are written; verify the router is running."
  fi
fi

if [ -n "$api_key" ]; then
  # Pass the router key via stdin (`@-`) instead of a -H argument so the key
  # never appears in the process arg list (visible via `ps` / /proc to other
  # local users on shared machines). We feed stdin via a small wrapper so the
  # spinner's exec form sees a single command argv.
  validate_key() {
    printf '%s: %s\n' "$router_key_header" "$api_key" \
      | curl -fsS --max-time 5 --header @- "$base_url/validate"
  }
  if ! spin "Validating API key" validate_key; then
    warn "Router rejected the API key (check it matches the dashboard at $base_url/ui/)."
  fi
fi

# ---------- done ----------

printf "\n"
printf "%s✓%s %s%sWeave Router installed for Claude Code.%s\n" \
  "$C_GREEN" "$C_RESET" "$C_BOLD" "$C_BRAND" "$C_RESET"
