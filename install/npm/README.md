# @workweave/router

One command, anywhere, to point Claude Code or Codex at the Weave Router.

```bash
npx @workweave/router                       # interactive: pick Claude Code or Codex, then scope
npx @workweave/router --claude              # skip the picker, target Claude Code
npx @workweave/router --codex               # skip the picker, target the OpenAI Codex CLI
npx @workweave/router --scope project       # per-repo install, commit settings.json (or .codex/)
npx @workweave/router --local               # self-hosted via docker-compose (localhost:8080)
npx @workweave/router --base-url https://router.acme.internal
npx @workweave/router --non-interactive     # reads $WEAVE_ROUTER_KEY, no prompts (defaults to claude)
```

Version-pin for reproducible setups:

```bash
npx @workweave/router@0.1.0 --claude --scope project
```

Uninstall:

```bash
npx @workweave/router --uninstall                       # Claude Code, user scope
npx @workweave/router --uninstall --codex               # Codex, user scope
npx @workweave/router --uninstall --scope project       # Claude Code, inside the repo
npx @workweave/router --uninstall --codex --scope project
```

## What it does

This package is a thin Node wrapper around [`install.sh`](./install.sh) from
the Weave Router repo. It exists so you can install from any machine with
Node ≥ 18 — no `curl | sh`, no Git clone, no PATH fiddling. Everything the
shell installer documents (targets, scopes, flags, environment variables)
works identically here.

Two install targets:

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

See the [main installer docs](https://github.com/workweave/router/tree/main/install)
for the full reference.

## Requirements

- Node ≥ 18 (ships with `npx`)
- `bash` on PATH (macOS / Linux native; Windows needs Git Bash or WSL)
- `jq` on PATH — used by the Claude Code status line script. Not required
  for the Codex path.

## Why npx

`curl -fsSL https://weave.ai/cc/install.sh | sh` still works and is fine.
`npx @workweave/router` adds: Windows support via Git Bash, painless version
pinning, no `curl | sh` aversion, and discoverability via the npm registry.
