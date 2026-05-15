# weave-router

One command, anywhere, to point Claude Code at the Weave Router.

```bash
npx weave-router                       # hosted router, user scope (interactive)
npx weave-router --scope project       # per-repo install, commit settings.json
npx weave-router --local               # self-hosted via docker-compose (localhost:8080)
npx weave-router --base-url https://router.acme.internal
npx weave-router --non-interactive     # reads $WEAVE_ROUTER_KEY, no prompts
```

Version-pin for reproducible setups:

```bash
npx weave-router@0.1.0 --scope project
```

Uninstall:

```bash
npx weave-router --uninstall                 # user scope
npx weave-router --uninstall --scope project # in the repo
```

## What it does

This package is a thin Node wrapper around [`install.sh`](./install.sh) from
the Weave Router repo. It exists so you can install from any machine with
Node ≥ 18 — no `curl | sh`, no Git clone, no PATH fiddling. Everything the
shell installer documents (scopes, flags, environment variables) works
identically here.

See the [main installer docs](https://github.com/workweave/router/tree/main/install)
for the full reference.

## Requirements

- Node ≥ 18 (ships with `npx`)
- `bash` on PATH (macOS / Linux native; Windows needs Git Bash or WSL)
- `jq` on PATH — used by the status line script

## Why npx

`curl -fsSL https://weave.ai/cc/install.sh | sh` still works and is fine.
`npx weave-router` adds: Windows support via Git Bash, painless version
pinning, no `curl | sh` aversion, and discoverability via the npm registry.
