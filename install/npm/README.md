# @workweave/router

One command, anywhere, to point Claude Code, Codex, or opencode at the Weave Router.

```bash
npx @workweave/router                       # interactive: pick Claude Code / Codex / opencode, then scope
npx @workweave/router --claude              # skip the picker, target Claude Code
npx @workweave/router --codex               # skip the picker, target the OpenAI Codex CLI
npx @workweave/router --opencode            # skip the picker, target opencode
npx @workweave/router --scope project       # per-repo install, commit settings.json (or .codex/ / opencode.json)
npx @workweave/router --local               # self-hosted via docker-compose (localhost:8080)
npx @workweave/router --base-url https://router.acme.internal
npx @workweave/router --non-interactive     # reads $WEAVE_ROUTER_KEY, no prompts (defaults to claude)
```

Version-pin for reproducible setups:

```bash
npx @workweave/router@0.1.0 --claude --scope project
```

Switch on/off without uninstalling (keeps your config so switching back is
instant; requires an explicit client):

```bash
npx @workweave/router off --claude      # route Claude Code directly to Anthropic
npx @workweave/router on --claude       # route Claude Code through the router again
npx @workweave/router status --codex    # is Codex on the router or direct?
```

Claude Code reads its router setting at launch, so quit and reopen it after an
on/off. Codex and opencode pick it up on their next run. Inside Claude Code the
slash commands `/router-off`, `/router-on`, and `/router-status` do the same.
Cursor has no config file we own — toggle its base URL override in **Settings →
Models** instead.

Uninstall:

```bash
npx @workweave/router --uninstall                       # Claude Code, user scope
npx @workweave/router --uninstall --codex               # Codex, user scope
npx @workweave/router --uninstall --opencode            # opencode, user scope
npx @workweave/router --uninstall --scope project       # Claude Code, inside the repo
npx @workweave/router --uninstall --codex --scope project
```

## What it does

This package is a thin Node wrapper around [`install.sh`](./install.sh) from
the Weave Router repo. It exists so you can install from any machine with
Node ≥ 18 — no `curl | sh`, no Git clone, no PATH fiddling. Everything the
shell installer documents (targets, scopes, flags, environment variables)
works identically here.

Three install targets:

- **Claude Code** (default) — patches `~/.claude/settings.json` (or
  `<repo>/.claude/settings.json` with `--scope project`) so `claude` routes
  through Weave automatically. Anthropic plan credentials flow through to
  api.anthropic.com.
- **Codex** (`--codex`) — patches `~/.codex/config.toml` (or
  `<repo>/.codex/config.toml`) with a managed `[model_providers.weave]`
  block plus `model_provider = "weave"`. OpenAI plan credentials flow
  through to api.openai.com. The block lives between begin/end markers so
  re-running the installer rewrites it cleanly and `--uninstall --codex`
  removes it without touching the rest of your config.
- **opencode** (`--opencode`) — merges a `provider.weave` entry (backed by
  opencode's built-in `@ai-sdk/anthropic` provider) into
  `~/.config/opencode/opencode.json` (or `<repo>/opencode.json` with
  `--scope project`). The router speaks the Anthropic Messages API
  natively, so opencode talks to it unmodified. Re-install rewrites only
  the managed `provider.weave` block; `--uninstall --opencode` strips it
  and leaves your other providers and settings alone.

See the [main installer docs](https://github.com/workweave/router/tree/main/install)
for the full reference.

## Requirements

- Node ≥ 18 (ships with `npx`)
- `bash` on PATH (macOS / Linux native; Windows needs Git Bash or WSL)
- `jq` on PATH — used by the Claude Code status line script and the
  opencode JSON merge. Not required for the Codex path.

## Why npx

`curl -fsSL https://weave.ai/cc/install.sh | sh` still works and is fine.
`npx @workweave/router` adds: Windows support via Git Bash, painless version
pinning, no `curl | sh` aversion, and discoverability via the npm registry.
