# opencode Weave Codex plugin

Lets a caller's own **ChatGPT (Codex) subscription** pay for their **opencode**
turns, routed through the Weave Router. Bundled into `@workweave/router`; the
installer (`--codex` / `--opencode`) drops `src/index.ts` into the user's
opencode plugins dir and writes a matching `weave-codex` provider block into
`opencode.json`.

## Why a plugin (config alone can't do it)

opencode removed built-in subscription auth in 1.3.0 and binds OAuth to its own
first-party providers — its bundled `openai/codex.ts` plugin hardcodes the
upstream to `chatgpt.com` and binds provider id `openai`, so a custom router
provider can't reuse it. And a subscription needs the caller's OAuth token in
`Authorization`, which expires hourly — a static `options.headers` string can't
refresh it.

This plugin re-implements the same ChatGPT OAuth + refresh against a **custom**
provider id (`weave-codex`) and, crucially, **leaves the request URL on the
Weave Router** instead of rewriting it to `chatgpt.com`.

## Wire shape it produces

Matches the router's `/v1/responses` Codex passthrough:

| | |
|---|---|
| `POST {router}/v1/responses` | Responses wire format (opencode's default for an `@ai-sdk/openai` provider) |
| `Authorization: Bearer <ChatGPT JWT>` | the caller's subscription, refreshed on expiry |
| `ChatGPT-Account-Id: <id>` | paired account id (required by the Codex backend) |
| `X-Weave-Router-Key: rk_…` | from `opencode.json` `options.headers` — the router authenticates off this, leaving `Authorization` free for the JWT |
| `originator`, `session-id` | from the plugin's `chat.headers` hook (Codex backend session continuity) |

The router detects the inbound Codex bearer and serves the turn on the caller's
own plan at the subscription fee.

## Division of responsibility

- **Installer** writes the router key + identity headers (`X-Weave-Router-Key`,
  `X-App`, `X-Weave-User-Email`) into `opencode.json` `options.headers`.
- **This plugin** manages only the dynamic, secret, refreshable subscription
  credential (the `Authorization` bearer + `ChatGPT-Account-Id`) via the auth
  `loader`, and the login flow via auth `methods` (browser PKCE + headless
  device code). Tokens are stored in opencode's own auth store under the
  `weave-codex` provider id.

## Env overrides

- `WEAVE_CODEX_OAUTH_ISSUER` — override the OpenAI auth issuer (self-hosted
  OpenAI auth proxies; also used by the capture tests).

## Verification

Capture-tested end-to-end against a local server standing in for the router
(opencode 1.17.9, real ChatGPT login): the inject path forwards the real JWT +
account-id in Responses format to `/v1/responses`, and the refresh path detects
an expired token, refreshes against the issuer, rotates + persists the tokens,
and injects the refreshed bearer. Typechecks under `strict` against
`@opencode-ai/plugin`.
