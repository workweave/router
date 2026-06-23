/**
 * @workweave/router — let a caller's own AI subscriptions pay for their opencode
 * turns, routed through the Weave Router.
 *
 * Model: a subscription is a CREDENTIAL scoped to the model family it can pay
 * for, not a provider you pick to force a model. You connect your ChatGPT
 * (Codex) and/or Claude (Pro/Max) plan once; the Weave Router routes every turn
 * to the best model and bills the plan that matches the model it served — and
 * only that. ChatGPT plan pays for GPT/Codex turns, Claude plan pays for Claude
 * turns, your Weave key pays for everything else. No manual provider-picking.
 *
 * opencode talks to one Responses-format `weave` provider; this plugin attaches
 * BOTH subscriptions to every request via the router's dedicated headers:
 *   - POST {router}/v1/responses                          (Responses wire format)
 *   - X-Weave-OpenAI-Subscription: <ChatGPT JWT>          (pays GPT/Codex turns)
 *   - X-Weave-OpenAI-Account-ID:   <account id>           (paired, Codex backend)
 *   - X-Weave-Anthropic-Subscription: <sk-ant-oat token>  (pays Claude turns)
 *   - X-Weave-Router-Key: rk_...                          (from config options.headers)
 *
 * The router authenticates off X-Weave-Router-Key, routes the turn across all
 * models the caller's subs + key can pay for, and resolves the matching
 * subscription per the chosen provider (so the ChatGPT plan can never be billed
 * for a Claude/OSS turn, and vice-versa).
 *
 * opencode stores one credential per provider id and the loader's getAuth() is
 * scoped to its own provider, so the two logins live in two slots:
 *   - provider `weave`        : the request provider; owns the ChatGPT login and
 *                               the loader that injects both subs.
 *   - provider `weave-claude`  : login-only; owns the Claude login. Its token is
 *                               read from opencode's on-disk auth store by the
 *                               `weave` loader (the SDK has no get-by-id).
 * Connecting ChatGPT activates sub-routing; the Claude sub then rides along when
 * present. With neither connected, `weave` is a plain router provider (your
 * Weave key pays) — the loader simply doesn't run.
 */

import type { Hooks, Plugin, PluginInput } from "@opencode-ai/plugin"
import { readFile } from "node:fs/promises"
import { homedir } from "node:os"
import { join } from "node:path"

// ---- ChatGPT (Codex) OAuth -------------------------------------------------
const CHATGPT_CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
// Overridable for self-hosted OpenAI auth proxies and for tests (mirrors the
// bundled codex plugin's `options.issuer`).
const CHATGPT_ISSUER = process.env.WEAVE_CODEX_OAUTH_ISSUER ?? "https://auth.openai.com"
const OAUTH_PORT = 1455
const OAUTH_POLLING_SAFETY_MARGIN_MS = 3000

// ---- Claude (Anthropic) OAuth ----------------------------------------------
// Canonical Claude Pro/Max OAuth (the same flow Claude Code uses): a manual
// code-paste browser flow. Authorize on claude.ai, exchange/refresh on the
// console token endpoint. The access token is an sk-ant-oat… subscription
// bearer; the router applies the Bearer + oauth beta header on the upstream leg.
const ANTHROPIC_CLIENT_ID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
const ANTHROPIC_AUTHORIZE_BASE = process.env.WEAVE_ANTHROPIC_OAUTH_AUTHORIZE ?? "https://claude.ai"
const ANTHROPIC_TOKEN_URL = process.env.WEAVE_ANTHROPIC_OAUTH_TOKEN ?? "https://console.anthropic.com/v1/oauth/token"
const ANTHROPIC_REDIRECT_URI = "https://console.anthropic.com/oauth/code/callback"
const ANTHROPIC_SCOPE = "org:create_api_key user:profile user:inference"

// Provider ids this plugin owns. `weave` is the request provider the installer
// writes into opencode.json; `weave-claude` is login-only storage for the Claude
// subscription. Deliberately NOT "openai"/"anthropic" — those ids are claimed by
// opencode's bundled provider plugins, which rewrite the upstream off the router.
const PROVIDER_ID = "weave"
const ANTHROPIC_PROVIDER_ID = "weave-claude"

// Dedicated router subscription headers. Must match the constants in
// internal/server/middleware/auth.go so the router stashes each sub and resolves
// it per the routed provider.
const HEADER_OPENAI_SUB = "X-Weave-OpenAI-Subscription"
const HEADER_OPENAI_ACCOUNT_ID = "X-Weave-OpenAI-Account-ID"
const HEADER_ANTHROPIC_SUB = "X-Weave-Anthropic-Subscription"

// Placeholder so the @ai-sdk/openai provider considers auth configured; the
// loader's fetch carries the real subscriptions in the dedicated headers and the
// router authenticates off X-Weave-Router-Key, so this value is never used.
const DUMMY_KEY = "weave-router-oauth"
const USER_AGENT = "weave-router-opencode"

interface PkceCodes {
  verifier: string
  challenge: string
}

function base64UrlEncode(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer)
  const binary = String.fromCharCode(...bytes)
  return btoa(binary)
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "")
}

async function generatePKCE(): Promise<PkceCodes> {
  // base64url of 32 random bytes → a 43-char verifier drawn uniformly from the
  // PKCE unreserved set (RFC 7636 §4.1). Encoding the raw bytes avoids the
  // modulo-on-a-CSPRNG bias that mapping bytes onto a 64-char alphabet would
  // introduce (and that static analysis flags).
  const verifier = base64UrlEncode(crypto.getRandomValues(new Uint8Array(32)).buffer)
  const challenge = base64UrlEncode(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier)))
  return { verifier, challenge }
}

interface IdTokenClaims {
  chatgpt_account_id?: string
  organizations?: Array<{ id: string }>
  "https://api.openai.com/auth"?: { chatgpt_account_id?: string }
}

function parseJwtClaims(token: string): IdTokenClaims | undefined {
  const parts = token.split(".")
  if (parts.length !== 3) return undefined
  try {
    return JSON.parse(Buffer.from(parts[1], "base64url").toString())
  } catch {
    return undefined
  }
}

function extractAccountIdFromClaims(claims: IdTokenClaims): string | undefined {
  return (
    claims.chatgpt_account_id ||
    claims["https://api.openai.com/auth"]?.chatgpt_account_id ||
    claims.organizations?.[0]?.id
  )
}

function extractAccountId(tokens: TokenResponse): string | undefined {
  if (tokens.id_token) {
    const claims = parseJwtClaims(tokens.id_token)
    const accountId = claims && extractAccountIdFromClaims(claims)
    if (accountId) return accountId
  }
  if (tokens.access_token) {
    const claims = parseJwtClaims(tokens.access_token)
    return claims ? extractAccountIdFromClaims(claims) : undefined
  }
  return undefined
}

interface TokenResponse {
  id_token: string
  access_token: string
  refresh_token: string
  expires_in?: number
}

function buildAuthorizeUrl(redirectUri: string, pkce: PkceCodes, state: string): string {
  const params = new URLSearchParams({
    response_type: "code",
    client_id: CHATGPT_CLIENT_ID,
    redirect_uri: redirectUri,
    scope: "openid profile email offline_access",
    code_challenge: pkce.challenge,
    code_challenge_method: "S256",
    id_token_add_organizations: "true",
    codex_cli_simplified_flow: "true",
    state,
    originator: "codex_cli_ts",
  })
  return `${CHATGPT_ISSUER}/oauth/authorize?${params.toString()}`
}

async function exchangeCodeForTokens(code: string, redirectUri: string, pkce: PkceCodes): Promise<TokenResponse> {
  const response = await fetch(`${CHATGPT_ISSUER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri,
      client_id: CHATGPT_CLIENT_ID,
      code_verifier: pkce.verifier,
    }).toString(),
  })
  if (!response.ok) throw new Error(`Token exchange failed: ${response.status}`)
  return response.json() as Promise<TokenResponse>
}

async function refreshAccessToken(refreshToken: string): Promise<TokenResponse> {
  const response = await fetch(`${CHATGPT_ISSUER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      refresh_token: refreshToken,
      client_id: CHATGPT_CLIENT_ID,
    }).toString(),
  })
  if (!response.ok) throw new Error(`Token refresh failed: ${response.status}`)
  return response.json() as Promise<TokenResponse>
}

// ---- Claude (Anthropic) OAuth helpers --------------------------------------

interface AnthropicTokens {
  access: string
  refresh: string
  expires: number
}

function buildAnthropicAuthorizeUrl(pkce: PkceCodes): string {
  const url = new URL(`${ANTHROPIC_AUTHORIZE_BASE}/oauth/authorize`)
  url.searchParams.set("code", "true")
  url.searchParams.set("client_id", ANTHROPIC_CLIENT_ID)
  url.searchParams.set("response_type", "code")
  url.searchParams.set("redirect_uri", ANTHROPIC_REDIRECT_URI)
  url.searchParams.set("scope", ANTHROPIC_SCOPE)
  url.searchParams.set("code_challenge", pkce.challenge)
  url.searchParams.set("code_challenge_method", "S256")
  // Claude Code reuses the PKCE verifier as the state value; the manual code is
  // returned to the user as "<code>#<state>".
  url.searchParams.set("state", pkce.verifier)
  return url.toString()
}

async function exchangeAnthropicCode(code: string, verifier: string): Promise<AnthropicTokens> {
  // The manual flow returns "<code>#<state>"; without the separator the state
  // would be undefined and the server would 4xx with an opaque error.
  if (!code.includes("#")) {
    throw new Error(`Expected the pasted code in "code#state" format; got: ${code.trim().slice(0, 24)}…`)
  }
  const [authCode, state] = code.trim().split("#")
  const response = await fetch(ANTHROPIC_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      grant_type: "authorization_code",
      code: authCode,
      state,
      client_id: ANTHROPIC_CLIENT_ID,
      redirect_uri: ANTHROPIC_REDIRECT_URI,
      code_verifier: verifier,
    }),
  })
  if (!response.ok) throw new Error(`Anthropic token exchange failed: ${response.status}`)
  const json = (await response.json()) as { access_token: string; refresh_token: string; expires_in?: number }
  return { access: json.access_token, refresh: json.refresh_token, expires: Date.now() + (json.expires_in ?? 3600) * 1000 }
}

async function refreshAnthropicToken(refreshToken: string): Promise<AnthropicTokens> {
  const response = await fetch(ANTHROPIC_TOKEN_URL, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      grant_type: "refresh_token",
      refresh_token: refreshToken,
      client_id: ANTHROPIC_CLIENT_ID,
    }),
  })
  if (!response.ok) throw new Error(`Anthropic token refresh failed: ${response.status}`)
  const json = (await response.json()) as { access_token: string; refresh_token?: string; expires_in?: number }
  return {
    access: json.access_token,
    // OAuth 2.0 lets the issuer omit a new refresh_token on refresh; keep the
    // existing one in that case rather than clearing it.
    refresh: json.refresh_token ?? refreshToken,
    expires: Date.now() + (json.expires_in ?? 3600) * 1000,
  }
}

// ---- opencode auth-store reader (cross-provider) ---------------------------
// The `weave` loader's getAuth() is scoped to `weave`, and the SDK exposes no
// get-by-id, so the Claude credential stored under `weave-claude` is read from
// opencode's on-disk auth store directly. Path mirrors opencode's Global.Path
// (XDG data home + /opencode/auth.json); overridable for tests.

interface StoredOAuth {
  type: string
  access?: string
  refresh?: string
  expires?: number
  accountId?: string
}

function opencodeAuthFile(): string {
  const override = process.env.WEAVE_OPENCODE_AUTH_FILE
  if (override) return override
  const dataHome = process.env.XDG_DATA_HOME || join(homedir(), ".local", "share")
  return join(dataHome, "opencode", "auth.json")
}

async function readStoredOAuth(providerID: string): Promise<StoredOAuth | undefined> {
  try {
    const raw = await readFile(opencodeAuthFile(), "utf8")
    const entry = (JSON.parse(raw) as Record<string, StoredOAuth>)[providerID]
    if (entry && entry.type === "oauth") return entry
  } catch {
    // No store yet / unreadable / not logged in — no Claude sub to attach.
  }
  return undefined
}

// ---- Browser OAuth loopback server (PKCE, ChatGPT) -------------------------

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] as string,
  )
}

const HTML_SUCCESS = `<!doctype html><html><head><title>Weave Router — authorized</title></head>
<body style="font-family:system-ui;background:#131010;color:#f1ecec;display:flex;justify-content:center;align-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1>Authorization successful</h1><p>You can close this window and return to opencode.</p>
<script>setTimeout(()=>window.close(),2000)</script></div></body></html>`

const renderOAuthError = (error: string) => `<!doctype html><html><head><title>Weave Router — authorization failed</title></head>
<body style="font-family:system-ui;background:#131010;color:#f1ecec;display:flex;justify-content:center;align-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1 style="color:#fc533a">Authorization failed</h1>
<div style="color:#ff917b;font-family:monospace;margin-top:1rem;padding:1rem;background:#3c140d;border-radius:.5rem">${escapeHtml(error)}</div></div></body></html>`

interface PendingOAuth {
  pkce: PkceCodes
  state: string
  resolve: (tokens: TokenResponse) => void
  reject: (error: Error) => void
}

let oauthServer: import("http").Server | undefined
let pendingOAuth: PendingOAuth | undefined

async function startOAuthServer(): Promise<{ redirectUri: string }> {
  const redirectUri = `http://localhost:${OAUTH_PORT}/auth/callback`
  if (oauthServer) return { redirectUri }
  const { createServer } = await import("node:http")
  oauthServer = createServer((req, res) => {
    const url = new URL(req.url || "/", `http://localhost:${OAUTH_PORT}`)
    if (url.pathname !== "/auth/callback") {
      res.writeHead(404)
      res.end("Not found")
      return
    }
    const code = url.searchParams.get("code")
    const state = url.searchParams.get("state")
    const error = url.searchParams.get("error_description") || url.searchParams.get("error")
    const fail = (status: number, msg: string) => {
      pendingOAuth?.reject(new Error(msg))
      pendingOAuth = undefined
      res.writeHead(status, { "Content-Type": "text/html; charset=utf-8" })
      res.end(renderOAuthError(msg))
    }
    if (error) return fail(200, error)
    if (!code) return fail(400, "Missing authorization code")
    if (!pendingOAuth || state !== pendingOAuth.state) return fail(400, "Invalid state - potential CSRF attack")
    const current = pendingOAuth
    pendingOAuth = undefined
    exchangeCodeForTokens(code, redirectUri, current.pkce)
      .then((tokens) => current.resolve(tokens))
      .catch((err) => current.reject(err))
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" })
    res.end(HTML_SUCCESS)
  })
  await new Promise<void>((resolve, reject) => {
    oauthServer!.listen(OAUTH_PORT, resolve)
    oauthServer!.on("error", reject)
  })
  return { redirectUri }
}

function stopOAuthServer(): void {
  oauthServer?.close(() => {})
  oauthServer = undefined
}

function waitForOAuthCallback(pkce: PkceCodes, state: string): Promise<TokenResponse> {
  // A new login supersedes any in-flight one: reject the old promise so it
  // can't hang or later clobber this flow's state.
  pendingOAuth?.reject(new Error("OAuth flow superseded by a new login"))
  return new Promise((resolve, reject) => {
    let entry: PendingOAuth | undefined
    // Each handler clears the shared slot only if it still owns it, so a stale
    // timer or callback never nulls out a newer flow's pendingOAuth.
    const clearIfOwner = () => {
      clearTimeout(timeout)
      if (pendingOAuth === entry) pendingOAuth = undefined
    }
    const timeout = setTimeout(() => {
      if (pendingOAuth === entry) {
        pendingOAuth = undefined
        reject(new Error("OAuth callback timeout - authorization took too long"))
      }
    }, 5 * 60 * 1000)
    entry = {
      pkce,
      state,
      resolve: (tokens) => {
        clearIfOwner()
        resolve(tokens)
      },
      reject: (error) => {
        clearIfOwner()
        reject(error)
      },
    }
    pendingOAuth = entry
  })
}

// ---- Request provider: `weave` (Responses, both subs) ----------------------

export const WeaveCodex: Plugin = async (input: PluginInput): Promise<Hooks> => {
  return {
    auth: {
      provider: PROVIDER_ID,
      async loader(getAuth) {
        const auth = await getAuth()
        if (auth.type !== "oauth") return {}

        // Coalesce concurrent refreshes (opencode fires parallel turns), one
        // in-flight promise per subscription.
        let chatgptRefresh: Promise<{ access: string; accountId: string | undefined }> | undefined
        let anthropicRefresh: Promise<string | undefined> | undefined

        // Resolve the ChatGPT (Codex) sub from this provider's own slot,
        // refreshing + persisting the rotated token on (or just before) expiry.
        async function resolveChatGPT(): Promise<{ access: string; accountId?: string } | undefined> {
          const current = (await getAuth()) as StoredOAuth
          if (current.type !== "oauth") return undefined
          // Use a still-valid access token regardless of whether a refresh token
          // is present (a partial store could lack it). Only an expired/absent
          // access needs a refresh — and that needs the refresh token.
          if (current.access && (current.expires ?? 0) >= Date.now()) {
            return { access: current.access, accountId: current.accountId }
          }
          // Access is expired/absent: a refresh is the only way forward, so
          // without a refresh token there's no usable credential.
          if (!current.refresh) return undefined
          if (!chatgptRefresh) {
            chatgptRefresh = refreshAccessToken(current.refresh)
              .then(async (tokens) => {
                const accountId = extractAccountId(tokens) || current.accountId
                await input.client.auth.set({
                  path: { id: PROVIDER_ID },
                  body: {
                    type: "oauth",
                    refresh: tokens.refresh_token || current.refresh!,
                    access: tokens.access_token,
                    expires: Date.now() + (tokens.expires_in ?? 3600) * 1000,
                    ...(accountId && { accountId }),
                  },
                })
                return { access: tokens.access_token, accountId }
              })
              .finally(() => {
                chatgptRefresh = undefined
              })
          }
          return chatgptRefresh
        }

        // Resolve the Claude sub from the `weave-claude` slot (read off disk),
        // refreshing + persisting the rotated token via the auth store. Returns
        // undefined when Claude isn't connected.
        async function resolveAnthropic(): Promise<string | undefined> {
          const current = await readStoredOAuth(ANTHROPIC_PROVIDER_ID)
          if (!current) return undefined
          if (current.access && (current.expires ?? 0) >= Date.now()) return current.access
          // Access is expired/absent: only a refresh can produce a live token,
          // so without a refresh token there's no usable credential to inject —
          // returning the stale token would have the router treat a dead Claude
          // sub as present instead of falling back to the Weave key. (Mirrors
          // resolveChatGPT.)
          if (!current.refresh) return undefined
          if (!anthropicRefresh) {
            anthropicRefresh = refreshAnthropicToken(current.refresh)
              .then(async (tokens) => {
                await input.client.auth.set({
                  path: { id: ANTHROPIC_PROVIDER_ID },
                  body: { type: "oauth", refresh: tokens.refresh, access: tokens.access, expires: tokens.expires },
                })
                return tokens.access
              })
              // A failed Claude refresh must not fail the turn — fall back to the
              // (possibly stale) token; an expired Claude turn the router can't
              // bill to the plan falls through to the Weave key on its end.
              .catch(() => current.access)
              .finally(() => {
                anthropicRefresh = undefined
              })
          }
          return anthropicRefresh
        }

        return {
          apiKey: DUMMY_KEY,
          async fetch(requestInput: RequestInfo | URL, init?: RequestInit) {
            // Preserve the configured headers (X-Weave-Router-Key, X-App, …) and
            // attach each connected subscription via its dedicated router header.
            // Authorization is left as the @ai-sdk placeholder; the router authes
            // off X-Weave-Router-Key and resolves the matching sub per the routed
            // model, so neither sub rides in Authorization.
            const headers = new Headers(init?.headers as HeadersInit | undefined)

            // Resolve both subs independently and never let one failure (e.g. a
            // failed ChatGPT token refresh) drop the other or fail the turn — a
            // Claude-routed turn must still get its sub when the ChatGPT refresh
            // is down, and vice-versa. Each resolver already persists rotations.
            const [chatgpt, anthropic] = await Promise.all([
              resolveChatGPT().catch(() => undefined),
              resolveAnthropic().catch(() => undefined),
            ])
            if (chatgpt?.access) {
              headers.set(HEADER_OPENAI_SUB, chatgpt.access)
              if (chatgpt.accountId) headers.set(HEADER_OPENAI_ACCOUNT_ID, chatgpt.accountId)
            }
            if (anthropic) headers.set(HEADER_ANTHROPIC_SUB, anthropic)

            return fetch(requestInput, { ...init, headers })
          },
        }
      },
      methods: [
        {
          label: "ChatGPT Pro/Plus — pays for GPT/Codex turns (browser)",
          type: "oauth",
          authorize: async () => {
            const { redirectUri } = await startOAuthServer()
            const pkce = await generatePKCE()
            const state = base64UrlEncode(crypto.getRandomValues(new Uint8Array(32)).buffer)
            const callbackPromise = waitForOAuthCallback(pkce, state)
            return {
              url: buildAuthorizeUrl(redirectUri, pkce, state),
              instructions: "Complete authorization in your browser. This window will close automatically.",
              method: "auto" as const,
              callback: async () => {
                const tokens = await callbackPromise
                stopOAuthServer()
                return {
                  type: "success" as const,
                  refresh: tokens.refresh_token,
                  access: tokens.access_token,
                  expires: Date.now() + (tokens.expires_in ?? 3600) * 1000,
                  accountId: extractAccountId(tokens),
                }
              },
            }
          },
        },
        {
          label: "ChatGPT Pro/Plus — pays for GPT/Codex turns (headless device code)",
          type: "oauth",
          authorize: async () => {
            const deviceResponse = await fetch(`${CHATGPT_ISSUER}/api/accounts/deviceauth/usercode`, {
              method: "POST",
              headers: { "Content-Type": "application/json", "User-Agent": USER_AGENT },
              body: JSON.stringify({ client_id: CHATGPT_CLIENT_ID }),
            })
            if (!deviceResponse.ok) throw new Error("Failed to initiate device authorization")
            const deviceData = (await deviceResponse.json()) as {
              device_auth_id: string
              user_code: string
              interval: string
            }
            const interval = Math.max(parseInt(deviceData.interval) || 5, 1) * 1000
            return {
              url: `${CHATGPT_ISSUER}/codex/device`,
              instructions: `Enter code: ${deviceData.user_code}`,
              method: "auto" as const,
              async callback() {
                const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
                while (true) {
                  const response = await fetch(`${CHATGPT_ISSUER}/api/accounts/deviceauth/token`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json", "User-Agent": USER_AGENT },
                    body: JSON.stringify({
                      device_auth_id: deviceData.device_auth_id,
                      user_code: deviceData.user_code,
                    }),
                  })
                  if (response.ok) {
                    const data = (await response.json()) as { authorization_code: string; code_verifier: string }
                    const tokenResponse = await fetch(`${CHATGPT_ISSUER}/oauth/token`, {
                      method: "POST",
                      headers: { "Content-Type": "application/x-www-form-urlencoded" },
                      body: new URLSearchParams({
                        grant_type: "authorization_code",
                        code: data.authorization_code,
                        redirect_uri: `${CHATGPT_ISSUER}/deviceauth/callback`,
                        client_id: CHATGPT_CLIENT_ID,
                        code_verifier: data.code_verifier,
                      }).toString(),
                    })
                    if (!tokenResponse.ok) throw new Error(`Token exchange failed: ${tokenResponse.status}`)
                    const tokens = (await tokenResponse.json()) as TokenResponse
                    return {
                      type: "success" as const,
                      refresh: tokens.refresh_token,
                      access: tokens.access_token,
                      expires: Date.now() + (tokens.expires_in ?? 3600) * 1000,
                      accountId: extractAccountId(tokens),
                    }
                  }
                  if (response.status !== 403 && response.status !== 404) return { type: "failed" as const }
                  await sleep(interval + OAUTH_POLLING_SAFETY_MARGIN_MS)
                }
              },
            }
          },
        },
      ],
    },
    // The Codex backend (which the router forwards GPT turns to) keys session
    // continuity off these headers; mirror opencode's bundled codex plugin.
    // Scoped to our provider so other providers are untouched.
    "chat.headers": async (hookInput, output) => {
      if (hookInput.model.providerID !== PROVIDER_ID) return
      output.headers["originator"] = "codex_cli_ts"
      output.headers["session-id"] = hookInput.sessionID
    },
    "chat.params": async (hookInput, output) => {
      if (hookInput.model.providerID !== PROVIDER_ID) return
      // Match codex cli: the Codex backend rejects an explicit max output cap.
      output.maxOutputTokens = undefined
    },
  }
}

// ---- Login-only provider: `weave-claude` (Claude Pro/Max) ------------------
// A second auth hook so the Claude subscription gets its own storage slot
// (opencode keys credentials by provider id). It serves no requests — the
// `weave` loader reads this slot and attaches the token — so it needs no loader.

export const WeaveClaude: Plugin = async (_input: PluginInput): Promise<Hooks> => {
  return {
    auth: {
      provider: ANTHROPIC_PROVIDER_ID,
      methods: [
        {
          label: "Claude Pro/Max — pays for Claude turns (browser)",
          type: "oauth",
          authorize: async () => {
            const pkce = await generatePKCE()
            return {
              url: buildAnthropicAuthorizeUrl(pkce),
              instructions: "Sign in with your Claude account, then paste the code shown (looks like `code#state`).",
              method: "code" as const,
              callback: async (code: string) => {
                const tokens = await exchangeAnthropicCode(code, pkce.verifier)
                return {
                  type: "success" as const,
                  refresh: tokens.refresh,
                  access: tokens.access,
                  expires: tokens.expires,
                }
              },
            }
          },
        },
      ],
    },
  }
}

export default WeaveCodex
