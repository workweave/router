# opencode Weave subscription plugin

Lets a caller's own **AI subscriptions** pay for their **opencode** turns, routed
through the Weave Router. A subscription is a **credential scoped to the model
family it can pay for**, not a provider you pick: you connect your ChatGPT
(Codex) and/or Claude (Pro/Max) plan once, the router routes every turn to the
best model, and bills the plan that matches the model it served — ChatGPT pays
for GPT/Codex turns, Claude pays for Claude turns, your Weave key pays for
everything else.

Bundled into `@workweave/router`; the installer (`--codex` / `--opencode`) drops
`src/index.ts` into the user's opencode plugins dir and writes a single
Responses-format `weave` provider (plus a login-only `weave-claude` provider)
into `opencode.json`.

## Why a plugin (config alone can't do it)

opencode removed built-in subscription auth in 1.3.0 and binds OAuth to its own
first-party providers, so a custom router provider can't reuse it. And
subscription tokens expire hourly — a static `options.headers` string can't
refresh them, nor carry two subscriptions whose tokens rotate independently.

## Wire shape it produces

opencode talks to one Responses-format `weave` provider; the plugin's loader
attaches whichever subscriptions are connected to **every** request via the
router's dedicated headers:

| | |
|---|---|
| `POST {router}/v1/responses` | Responses wire format (opencode's default for an `@ai-sdk/openai` provider) |
| `X-Weave-OpenAI-Subscription: <ChatGPT JWT>` | pays GPT/Codex turns, refreshed on expiry |
| `X-Weave-OpenAI-Account-ID: <id>` | paired account id (required by the Codex backend) |
| `X-Weave-Anthropic-Subscription: <sk-ant-oat token>` | pays Claude turns, refreshed on expiry |
| `X-Weave-Router-Key: rk_…` | from `opencode.json` `options.headers` — the router authenticates off this |

The router routes the turn across every model the caller's subs + key can pay
for and resolves the subscription matching the chosen provider, so a sub is
never billed for a turn outside its family.

## Two storage slots, one request provider

opencode stores one credential per provider id and the loader's `getAuth()` is
scoped to its own provider, so the two logins live in two slots:

- **`weave`** — the request provider. Owns the **ChatGPT** login and the loader
  that attaches both subscriptions. Connecting ChatGPT activates sub-routing.
- **`weave-claude`** — login-only (no models, serves no requests). Owns the
  **Claude** login. Its token is read from opencode's on-disk auth store by the
  `weave` loader (the SDK exposes no get-by-id).

With neither connected, `weave` is a plain router provider (your Weave key pays)
— the loader simply doesn't run. Connecting ChatGPT is what turns on
subscription routing; the Claude sub then rides along when present.

## Login

`opencode auth login` → **Weave Router** → *ChatGPT Pro/Plus* (browser or
headless device code) and/or **Weave Router — Claude plan** → *Claude Pro/Max*
(browser; paste the `code#state` shown after authorizing).

## Env overrides (self-host + tests)

- `WEAVE_CODEX_OAUTH_ISSUER` — OpenAI auth issuer.
- `WEAVE_ANTHROPIC_OAUTH_AUTHORIZE` / `WEAVE_ANTHROPIC_OAUTH_TOKEN` — Anthropic
  OAuth authorize host / token endpoint.
- `WEAVE_OPENCODE_AUTH_FILE` — path to opencode's `auth.json` (the `weave`
  loader reads the `weave-claude` slot from here; defaults to
  `$XDG_DATA_HOME/opencode/auth.json`).

## Verification

`bun test test/` (run under bun, opencode's own runtime) covers: dual-sub
injection via the dedicated headers with the router key preserved and
`Authorization` left clean; ChatGPT-only graceful degradation; expired Claude
token refresh + persist + rotated injection; the loader staying inert without
oauth; and the Claude login hook's canonical OAuth flow + `code#state` exchange.
Typechecks under `strict` against `@opencode-ai/plugin`.
