# Weave Router — Claude Code + Codex installer

One command to point Claude Code or the OpenAI Codex CLI at the Weave Router
permanently. No shell exports, no manual config edits.

## Quick start

### Hosted Weave Router

```bash
# Interactive: the installer asks Claude Code or Codex, then user vs. project
npx @workweave/router

# Skip the target picker:
npx @workweave/router --claude                     # Claude Code, user scope
npx @workweave/router --codex                      # Codex, user scope

# Project scope — only when running inside this repo:
npx @workweave/router --claude --scope project     # Claude Code
npx @workweave/router --codex  --scope project     # Codex
```

Prefer `curl`? The same installer is also served as a shell script:

```bash
curl -fsSL https://weave.ai/cc/install.sh | sh
curl -fsSL https://weave.ai/cc/install.sh | sh -s -- --codex
curl -fsSL https://weave.ai/cc/install.sh | sh -s -- --scope project
```

Or from a clone of this repo:

```bash
./router/install/install.sh                    # prompts: target, then scope
./router/install/install.sh --claude           # skip picker, Claude Code
./router/install/install.sh --codex            # skip picker, Codex
./router/install/install.sh --scope project    # team install
```

When run interactively without `--claude` / `--codex`, the installer asks
which tool to target (defaults to Claude Code on Enter). Without `--scope`,
it then asks user vs. project (defaults to user). `--non-interactive` skips
both prompts (target defaults to Claude Code) — useful for CI and
`curl | sh` pipelines.

The installer also prompts for your API key (or reads `$WEAVE_ROUTER_KEY`
for non-interactive installs).

### Self-hosted via `docker compose` (zero-config)

If you're running the router locally with the bundled `docker-compose.yml`
(`localhost:8080`), use the shortcut:

```bash
cd router
make full-setup                 # boot the stack and seed a router key
make install-cc                 # → ./install/install.sh --claude --local
claude                          # routes through your local router
```

`make install-cc` is a wrapper around `./install/install.sh --claude --local`,
which is shorthand for `--base-url http://localhost:8080`. For Codex, swap
the target flag:

```bash
./router/install/install.sh --codex --local                    # user scope Codex
./router/install/install.sh --codex --local --scope project    # team scope Codex
```

### Self-hosted on a custom URL

```bash
# Internal deploy with seeded keys (will prompt for the bearer):
./router/install/install.sh --base-url https://router.your-company.internal

# Custom local port, dev mode:
./router/install/install.sh --base-url http://localhost:9000 --dev-mode
```

## What gets written

### Claude Code (`--claude`, default)

**User scope:**

| Path                                  | Purpose                                                       |
| ------------------------------------- | ------------------------------------------------------------- |
| `~/.claude/settings.json`             | Sets `env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_CUSTOM_HEADERS` with `X-Weave-Router-Key`, and `statusLine`. Other keys preserved. |
| `~/.weave/cc-statusline.sh`           | The status line script. Reads the router's decisions log + the CC transcript to show routed-model + savings. |

**Project scope (`--scope project`):**

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

### Codex (`--codex`)

**User scope:**

| Path                       | Purpose                                                       |
| -------------------------- | ------------------------------------------------------------- |
| `~/.codex/config.toml`     | Adds a managed `[model_providers.weave]` block + sets top-level `model_provider = "weave"`, both between `# >>> weave-router managed` markers. Anything outside the markers is preserved. |

**Project scope (`--scope project`):**

| Path                             | Committed? | Purpose                                                       |
| -------------------------------- | ---------- | ------------------------------------------------------------- |
| `<repo>/.codex/config.toml`      | ❌ ignored | Per-teammate config (holds the router key). Each teammate runs the installer for their own key. |
| `<repo>/.gitignore`              | ✅ commit  | Adds `.codex/config.toml` to the ignore list.                  |

Run Codex from the repo with `CODEX_HOME=<repo>/.codex codex` so it picks
up the project-local config instead of `~/.codex/`.

Re-running the installer rewrites only the managed block (TOML between the
markers + a top-level `model_provider =` outside it). Everything else —
profiles, alternate providers, comments — stays untouched.

**Onboarding flow for a new teammate (either target):**

```bash
git clone <repo>
cd <repo>
npx @workweave/router --claude --scope project   # or --codex
export WEAVE_ROUTER_KEY=rk_...                    # in shell rc / dotenv / 1Password
claude                                             # or `CODEX_HOME=.codex codex`
```

The `--scope project` step only needs to run once per checkout (re-run if
`cc-statusline.sh` is updated upstream).

## Flags

| Flag                       | Default                       | Meaning                                                                |
| -------------------------- | ----------------------------- | ---------------------------------------------------------------------- |
| `--claude`                 | (target picker if interactive) | Skip the target picker; install for Claude Code.                       |
| `--codex`                  | (target picker if interactive) | Skip the target picker; install for the OpenAI Codex CLI.              |
| `--scope user\|project`    | interactive prompt (default `user`) | User-level install (everywhere) vs project-level (this repo only).      |
| `--local`                  | off                           | Shortcut for the bundled docker-compose router (`localhost:8080`).      |
| `--base-url <url>`         | `https://router.workweave.ai` | Override the router endpoint. Use for self-hosted / custom port.        |
| `--non-interactive`        | off                           | Fail if `$WEAVE_ROUTER_KEY` isn't set instead of prompting. Defaults target to Claude Code so existing CI pipelines don't shift semantics. |

Override the default base URL globally by setting `$WEAVE_ROUTER_URL` before
running the installer.

## Verifying

**Claude Code:**

1. Run `claude`. The status line at the bottom should show
   `WEAVE ROUTER — <routed-model> ← <selected-model>` after one turn.
2. After several turns it should add `· saved $X turn / $Y session`.
3. Check `~/.weave-router/decisions.jsonl` — one row per request.

If the status line never appears, run `claude --debug` and check stderr for
errors invoking `cc-statusline.sh`. The script needs `jq` on PATH.

**Codex:**

1. Open `~/.codex/config.toml` (or `<repo>/.codex/config.toml` for project
   scope) and confirm the `# >>> weave-router managed >>>` block exists with
   your `X-Weave-Router-Key`.
2. Run `codex` and issue a turn. Provider should be `Weave Router`.
3. Check the router's dashboard at `<base-url>/ui/dashboard` to see the
   routed decision.

## Uninstall

```bash
npx @workweave/router --uninstall                       # Claude Code, user scope
npx @workweave/router --uninstall --codex               # Codex, user scope
npx @workweave/router --uninstall --scope project       # Claude Code, in the repo
npx @workweave/router --uninstall --codex --scope project
```

Removes only the keys / block this installer added; everything else in
`settings.json` / `config.toml` is left alone.
