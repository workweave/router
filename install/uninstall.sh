#!/usr/bin/env bash
#
# Weave Router uninstaller for Claude Code and Codex.
#
# Default target is Claude Code: removes the env vars, statusLine, and local
# router auth that install.sh added; leaves the rest of settings.json
# untouched. Pass --codex to strip the managed [model_providers.weave]
# block (and the matching top-level `model_provider`) from Codex's
# config.toml — anything outside the markers is preserved.
#
# Usage:
#   npx @workweave/router --uninstall                            # Claude Code, user scope
#   npx @workweave/router --uninstall --codex                    # Codex, user scope
#   npx @workweave/router --uninstall --scope project            # run inside the repo
#   npx @workweave/router --uninstall --dir /tmp/test            # --dir alone (user scope, .weave/)
#   npx @workweave/router --uninstall --scope project --dir /tmp # --dir + project scope (.claude/)

set -euo pipefail

scope="user"
scope_explicit="false"
install_dir=""
target="claude"

err()  { printf "\033[31merror:\033[0m %s\n" "$*" >&2; }
info() { printf "\033[36m==>\033[0m %s\n" "$*"; }
ok()   { printf "\033[32m✓\033[0m %s\n" "$*"; }

# Refuse to write/delete through a symlink. Project scope reads paths from the
# user's git repo; a hostile checkout could ship `.claude/settings.json` (or
# `.claude/cc-statusline.sh`) as a symlink to e.g. `~/.ssh/authorized_keys`,
# and the uninstaller's `>` redirect or `rm` would silently follow that link.
refuse_if_symlink() {
  local target="$1"
  if [ -L "$target" ]; then
    err "$target is a symlink (-> $(readlink "$target")). Refusing to operate on it."
    exit 1
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --scope)
      scope="${2:-}"; shift 2
      [ "$scope" = "user" ] || [ "$scope" = "project" ] || { err "--scope must be 'user' or 'project'"; exit 2; }
      scope_explicit="true"
      ;;
    --dir)
      install_dir="${2:-}"; shift 2
      [ -n "$install_dir" ] || { err "--dir requires a path"; exit 2; }
      ;;
    --codex)
      target="codex"; shift
      ;;
    -h|--help)
      # awk avoids GNU `head -n -<N>` (rejected by BSD head on macOS).
      awk 'NR<2 { next } /^set -euo/ { exit } { sub(/^# ?/, ""); print }' "$0"
      exit 0
      ;;
    *)
      err "unknown flag: $1"; exit 2
      ;;
  esac
done

if [ "$target" = "claude" ] && ! command -v jq >/dev/null 2>&1; then
  err "jq is required for the Claude Code uninstall path."
  exit 1
fi

# Markers must stay in sync with install.sh. Keep verbatim.
WEAVE_CODEX_BEGIN_MARKER="# >>> weave-router managed (do not edit between markers) >>>"
WEAVE_CODEX_END_MARKER="# <<< weave-router managed <<<"

# strip_codex_block rewrites config.toml without the managed block and any
# top-level `model_provider = "weave"` that lived outside the markers (which
# can happen if the user copy-pasted our key into their own config). Other
# top-level model_provider values are preserved so we don't yank a user back
# into the OpenAI default when they meant to keep their own.
strip_codex_block() {
  local config_file="$1"
  local tmp; tmp="$(mktemp -t weave-codex-uninstall.XXXXXX)"
  awk -v begin="$WEAVE_CODEX_BEGIN_MARKER" -v end="$WEAVE_CODEX_END_MARKER" '
    $0 == begin { skip = 1; next }
    $0 == end   { skip = 0; next }
    skip        { next }
    /^[[:space:]]*\[/ { in_section = 1 }
    !in_section && /^[[:space:]]*model_provider[[:space:]]*=[[:space:]]*"weave"[[:space:]]*$/ { next }
    { print }
  ' "$config_file" >"$tmp"
  mv "$tmp" "$config_file"
}

# ---------- codex uninstall path ----------

if [ "$target" = "codex" ]; then
  # Resolve codex_config_file based on scope/dir. Mirrors the install path so
  # an `install --codex --scope project` is exactly reversed by an
  # `uninstall --codex --scope project`.
  if [ -n "$install_dir" ]; then
    install_dir="$(cd "$install_dir" 2>/dev/null && pwd || echo "$install_dir")"
    codex_dir="$install_dir/.codex"
    refuse_if_symlink "$codex_dir"
  elif [ "$scope" = "user" ]; then
    codex_dir="$HOME/.codex"
  else
    # Project scope: same prompt + git-root fallback as install.sh.
    project_dir=""
    if [ "$scope_explicit" = "false" ] && [ -r /dev/tty ]; then
      default_project_dir="$(pwd)"
      printf "Project directory to uninstall from [default: %s]: " "$default_project_dir"
      read -r project_dir_choice </dev/tty || project_dir_choice=""
      project_dir="${project_dir_choice:-$default_project_dir}"
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
    if [ -n "${project_dir:-}" ]; then
      codex_dir="$project_dir/.codex"
    else
      if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
        err "--scope project must be run inside a git repo, or use --dir <path>."
        exit 1
      fi
      codex_dir="$git_root/.codex"
    fi
    refuse_if_symlink "$codex_dir"
  fi
  codex_config_file="$codex_dir/config.toml"
  refuse_if_symlink "$codex_config_file"

  if [ -f "$codex_config_file" ]; then
    strip_codex_block "$codex_config_file"
    # If the file now contains only whitespace/comments, leave it: the user
    # may have other comments worth keeping. Truly empty files we delete so
    # we don't leave a zero-byte artifact behind.
    if [ ! -s "$codex_config_file" ]; then
      rm -f "$codex_config_file"
      ok "Removed empty $codex_config_file"
    else
      ok "Cleaned $codex_config_file"
    fi
  else
    info "No Codex config at $codex_config_file (already uninstalled?)"
  fi

  if [ -n "$install_dir" ]; then
    ok "Weave Router uninstalled from $install_dir (Codex)."
  else
    ok "Weave Router uninstalled (Codex, scope=$scope)."
  fi
  exit 0
fi

# ---------- claude uninstall path ----------

# Resolve the base directory. When --dir is given, use it directly.
if [ -n "$install_dir" ]; then
  install_dir="$(cd "$install_dir" 2>/dev/null && pwd || echo "$install_dir")"
  settings_file="$install_dir/.claude/settings.json"
  local_settings_file=""
  # --dir alone (scope defaults to "user") uses .weave/; --dir --scope project
  # uses .claude/. Match the installer's scope-dependent statusline placement.
  if [ "$scope" = "project" ]; then
    statusline_file="$install_dir/.claude/cc-statusline.sh"
  else
    statusline_file="$install_dir/.weave/cc-statusline.sh"
  fi
  # Symlink containment: --dir paths come from a user-supplied directory that may
  # be hostile. The later `>` redirect on settings_file and `rm -f` on the
  # statusline script would otherwise follow links out of the directory.
  refuse_if_symlink "$install_dir/.claude"
  refuse_if_symlink "$settings_file"
  refuse_if_symlink "$statusline_file"
elif [ "$scope" = "user" ]; then
  settings_file="$HOME/.claude/settings.json"
  local_settings_file=""
  statusline_file="$HOME/.weave/cc-statusline.sh"
else
  # Project scope without --dir: mirror install.sh — directory prompt only when
  # scope_explicit is false (interactive install path); explicit --scope project
  # uses the git root of CWD with no prompt.
  project_dir=""
  if [ "$scope_explicit" = "false" ] && [ -r /dev/tty ]; then
    default_project_dir="$(pwd)"
    printf "Project directory to uninstall from [default: %s]: " "$default_project_dir"
    read -r project_dir_choice </dev/tty || project_dir_choice=""
    project_dir="${project_dir_choice:-$default_project_dir}"
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
  if [ -n "${project_dir:-}" ]; then
    settings_base="$project_dir"
  else
    if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
      err "--scope project must be run inside a git repo, or use --dir <path>."
      exit 1
    fi
    settings_base="$git_root"
  fi
  settings_file="$settings_base/.claude/settings.json"
  local_settings_file="$settings_base/.claude/settings.local.json"
  statusline_file="$settings_base/.claude/cc-statusline.sh"
  # Symlink containment: paths come from a git repo or user-supplied directory
  # that may be hostile. The later `>` redirect on settings_file and `rm -f` on
  # the scripts would otherwise follow links out of the repo.
  refuse_if_symlink "$settings_base/.claude"
  refuse_if_symlink "$settings_file"
  refuse_if_symlink "$local_settings_file"
  refuse_if_symlink "$statusline_file"
fi

if [ -f "$settings_file" ]; then
  # Only remove keys we actually installed: scrub our two env vars, and only
  # delete `statusLine` / `apiKeyHelper` when they point at scripts this
  # installer used in older versions. Otherwise an unrelated user-configured
  # statusLine or apiKeyHelper would be silently clobbered.
  cleaned="$(jq '
    if .env then
      .env |= (del(.ANTHROPIC_BASE_URL, .ANTHROPIC_AUTH_TOKEN, .ANTHROPIC_CUSTOM_HEADERS))
      | (if (.env | length) == 0 then del(.env) else . end)
    else . end
    | (if (.statusLine.command // "" | tostring | endswith("cc-statusline.sh"))
         then del(.statusLine) else . end)
    | (if (.apiKeyHelper // "" | tostring | endswith("weave-key.sh"))
         then del(.apiKeyHelper) else . end)
  ' "$settings_file")"
  printf '%s\n' "$cleaned" >"$settings_file"
  ok "Cleaned $settings_file"
else
  info "No settings file at $settings_file (already uninstalled?)"
fi

if [ -n "$local_settings_file" ] && [ -f "$local_settings_file" ]; then
  cleaned="$(jq '
    if .env then
      .env |= (del(.ANTHROPIC_AUTH_TOKEN, .ANTHROPIC_CUSTOM_HEADERS))
      | (if (.env | length) == 0 then del(.env) else . end)
    else . end
    | del(.apiKeyHelper)
  ' "$local_settings_file")"
  printf '%s\n' "$cleaned" >"$local_settings_file"
  ok "Cleaned $local_settings_file"
fi

for f in "$statusline_file"; do
  if [ -f "$f" ]; then
    rm -f "$f"
    ok "Removed $f"
  fi
done

if [ -n "$install_dir" ]; then
  ok "Weave Router uninstalled from $install_dir."
else
  ok "Weave Router uninstalled (scope=$scope)."
fi
