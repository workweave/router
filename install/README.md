# Weave Router — Claude Code installer

One command to point Claude Code at the Weave Router permanently and turn on
the routed-model status line. No shell exports, no manual `settings.json`
edits.

## Quick start

### Hosted Weave Router

```bash
# User scope — applies everywhere on this machine
npx weave-router

# Project scope — only when running `claude` inside this repo
npx weave-router --scope project
```

Prefer `curl`? The same installer is also served as a shell script:

```bash
curl -fsSL https://weave.ai/cc/install.sh | sh
curl -fsSL https://weave.ai/cc/install.sh | sh -s -- --scope project
```

Or from a clone of this repo:

```bash
./router/install/install.sh                  # prompts: user or project
./router/install/install.sh --scope user     # skip prompt — user scope
./router/install/install.sh --scope project  # skip prompt — project scope
```

When run interactively without `--scope`, the installer asks whether to install
at **user** scope (everywhere) or **project** scope (this repo only) and
defaults to `user` on Enter. Pass `--scope` explicitly (or `--non-interactive`)
to skip the prompt — useful for CI and `curl | sh` pipelines.

The installer also prompts for your API key (or reads `$WEAVE_ROUTER_KEY` for
non-interactive installs).

### Self-hosted via `docker compose` (zero-config)

If you're running the router locally with the bundled `docker-compose.yml`
(`localhost:8080`), use the shortcut:

```bash
cd router
make full-setup                 # boot the stack and seed a router key
make install-cc                 # → ./install/install.sh --local
claude                          # routes through your local router
```

`make install-cc` is a wrapper around `./install/install.sh --local`,
which is shorthand for `--base-url http://localhost:8080`. Use the long
form if you want to mix-and-match flags (e.g. project scope on a local router):

```bash
./router/install/install.sh --local --scope project
```

### Self-hosted on a custom URL

```bash
# Internal deploy with seeded keys (will prompt for the bearer):
./router/install/install.sh --base-url https://router.your-company.internal

# Custom local port, dev mode:
./router/install/install.sh --base-url http://localhost:9000 --dev-mode
```

## What gets written

### User scope (default)

| Path                                  | Purpose                                                       |
| ------------------------------------- | ------------------------------------------------------------- |
| `~/.claude/settings.json`             | Sets `env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_CUSTOM_HEADERS` with `X-Weave-Router-Key`, and `statusLine`. Other keys preserved. |
| `~/.weave/cc-statusline.sh`           | The status line script. Reads the router's decisions log + the CC transcript to show routed-model + savings. |

Re-running the installer overwrites those keys idempotently. Other settings
(hooks, plugins, theme, etc.) are merged, not clobbered.

### Project scope (`--scope project`)

| Path                                | Committed? | Purpose                                                       |
| ----------------------------------- | ---------- | ------------------------------------------------------------- |
| `<repo>/.claude/settings.json`      | ✅ commit  | Sets `env.ANTHROPIC_BASE_URL`, `statusLine` (relative paths). **No token.** |
| `<repo>/.gitignore`                 | ✅ commit  | Adds the four `.claude/` paths below to the ignore list.       |
| `<repo>/.claude/cc-statusline.sh`   | ❌ ignored | Status line script — runs on every CC session.                 |
| `<repo>/.claude/settings.local.json`| ❌ ignored | Stores your local `ANTHROPIC_CUSTOM_HEADERS` router-key header and any other per-teammate overrides. |
| `<repo>/.claude/.credentials.json`  | ❌ ignored | CC's per-user credentials cache.                               |

The router key lives in `ANTHROPIC_CUSTOM_HEADERS` so Claude Code can keep
using its normal Anthropic auth (`Authorization` / `x-api-key`) for the
logged-in user's Team/Pro/Max/individual plan.

**Onboarding flow for a new teammate:**

```bash
git clone <repo>
cd <repo>
./router/install/install.sh --scope project   # writes shared settings + local router key header
export WEAVE_ROUTER_KEY=rk_...                 # in shell rc / dotenv / 1Password
claude                                          # routes through Weave
```

The `install.sh --scope project` step only needs to run once per checkout
(re-run if `cc-statusline.sh` is updated upstream).

## Flags

| Flag                       | Default                       | Meaning                                                                |
| -------------------------- | ----------------------------- | ---------------------------------------------------------------------- |
| `--scope user\|project`    | interactive prompt (default `user`) | User-level install (everywhere) vs project-level (this repo only). If omitted on a TTY, the installer asks; defaults to `user` non-interactively. |
| `--local`                  | off                           | Shortcut for the bundled docker-compose router (`localhost:8080`).      |
| `--base-url <url>`         | `https://router.workweave.ai` | Override the router endpoint. Use for self-hosted / custom port.        |
| `--non-interactive`        | off                           | Fail if `$WEAVE_ROUTER_KEY` isn't set instead of prompting. CI-friendly. |

Override the default base URL globally by setting `$WEAVE_ROUTER_URL` before
running the installer.

## Verifying

After install:

1. Run `claude`. The status line at the bottom should show
   `WEAVE ROUTER — <routed-model> ← <selected-model>` after one turn.
2. After several turns it should add `· saved $X turn / $Y session`.
3. Check `~/.weave-router/decisions.jsonl` — one row per request.

If the status line never appears, run `claude --debug` and check stderr for
errors invoking `cc-statusline.sh`. The script needs `jq` on PATH.

## Uninstall

```bash
./router/install/uninstall.sh                  # user scope
./router/install/uninstall.sh --scope project  # in the repo
```

Removes only the keys this installer added; leaves everything else in
`settings.json` alone.
