#!/usr/bin/env bash
#
# Weave Router uninstaller for Claude Code.
#
# Removes the env vars, statusLine, and local router auth that install.sh added.
# Leaves the rest of settings.json untouched.
#
# Usage:
#   ./uninstall.sh                  # user scope
#   ./uninstall.sh --scope project  # run inside the repo
#   ./uninstall.sh --dir /tmp/test  # remove from arbitrary directory

set -euo pipefail

scope="user"
install_dir=""

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
      ;;
    --dir)
      install_dir="${2:-}"; shift 2
      [ -n "$install_dir" ] || { err "--dir requires a path"; exit 2; }
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

if ! command -v jq >/dev/null 2>&1; then
  err "jq is required."
  exit 1
fi

# Resolve the base directory. When --dir is given, use it directly.
if [ -n "$install_dir" ]; then
  install_dir="$(cd "$install_dir" 2>/dev/null && pwd || echo "$install_dir")"
  settings_file="$install_dir/.claude/settings.json"
  local_settings_file=""
  statusline_file="$install_dir/.claude/cc-statusline.sh"
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
  # project
  if ! git_root="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    err "--scope project must be run inside a git repo, or use --dir <path>."
    exit 1
  fi
  settings_file="$git_root/.claude/settings.json"
  local_settings_file="$git_root/.claude/settings.local.json"
  statusline_file="$git_root/.claude/cc-statusline.sh"
  # Symlink containment: paths come from a git repo that may be hostile. The
  # later `>` redirect on settings_file and `rm -f` on the scripts would
  # otherwise follow links out of the repo.
  refuse_if_symlink "$git_root/.claude"
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
