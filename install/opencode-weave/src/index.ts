/**
 * @workweave/router — use a caller's ChatGPT (Codex) subscription for their
 * opencode turns, routed through the Weave Router.
 *
 * opencode removed built-in subscription auth in 1.3.0 and binds OAuth to its
 * own first-party providers (the bundled `openai/codex.ts` plugin hardcodes the
 * upstream to chatgpt.com and binds provider "openai"), so a custom router
 * provider can't reuse it. This plugin re-implements the same ChatGPT OAuth +
 * refresh against a CUSTOM provider id (`weave-codex`) and, crucially, leaves
 * the request URL pointed at the Weave Router instead of chatgpt.com.
 *
 * Wire shape produced (matches the router's /v1/responses Codex passthrough):
 *   - POST {router}/v1/responses                       (Responses wire format)
 *   - Authorization: Bearer <ChatGPT JWT>              (the caller's subscription)
 *   - ChatGPT-Account-Id: <account id>                 (paired, required by Codex backend)
 *   - X-Weave-Router-Key: rk_...                       (from config options.headers)
 *
 * The router authenticates off X-Weave-Router-Key (so Authorization is free for
 * the subscription JWT), detects the inbound Codex bearer, and serves the turn
 * on the caller's own plan at the subscription fee. The router key + identity
 * headers (X-App, X-Weave-User-Email) live in opencode.json `options.headers`
 * written by the installer; this plugin manages only the dynamic, refreshable
 * subscription credential.
 */

import type { Hooks, Plugin, PluginInput } from "@opencode-ai/plugin"

const CLIENT_ID = "app_EMoamEEZ73f0CkXaXp7hrann"
// Overridable for self-hosted OpenAI auth proxies and for tests (mirrors the
// bundled codex plugin's `options.issuer`).
const ISSUER = process.env.WEAVE_CODEX_OAUTH_ISSUER ?? "https://auth.openai.com"
const OAUTH_PORT = 1455
const OAUTH_POLLING_SAFETY_MARGIN_MS = 3000
// Provider id this plugin owns. Must match the provider block the installer
// writes into opencode.json. Deliberately NOT "openai" — that id is claimed by
// opencode's bundled codex plugin, which rewrites the upstream to chatgpt.com.
const PROVIDER_ID = "weave-codex"
// Placeholder so the @ai-sdk/openai provider considers auth configured; the
// loader's fetch overwrites Authorization with the real subscription bearer.
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
    client_id: CLIENT_ID,
    redirect_uri: redirectUri,
    scope: "openid profile email offline_access",
    code_challenge: pkce.challenge,
    code_challenge_method: "S256",
    id_token_add_organizations: "true",
    codex_cli_simplified_flow: "true",
    state,
    originator: "codex_cli_ts",
  })
  return `${ISSUER}/oauth/authorize?${params.toString()}`
}

async function exchangeCodeForTokens(code: string, redirectUri: string, pkce: PkceCodes): Promise<TokenResponse> {
  const response = await fetch(`${ISSUER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "authorization_code",
      code,
      redirect_uri: redirectUri,
      client_id: CLIENT_ID,
      code_verifier: pkce.verifier,
    }).toString(),
  })
  if (!response.ok) throw new Error(`Token exchange failed: ${response.status}`)
  return response.json() as Promise<TokenResponse>
}

async function refreshAccessToken(refreshToken: string): Promise<TokenResponse> {
  const response = await fetch(`${ISSUER}/oauth/token`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      refresh_token: refreshToken,
      client_id: CLIENT_ID,
    }).toString(),
  })
  if (!response.ok) throw new Error(`Token refresh failed: ${response.status}`)
  return response.json() as Promise<TokenResponse>
}

// ---- Browser OAuth loopback server (PKCE) --------------------------------

function escapeHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c] as string,
  )
}

const HTML_SUCCESS = `<!doctype html><html><head><title>Weave Router — Codex authorized</title></head>
<body style="font-family:system-ui;background:#131010;color:#f1ecec;display:flex;justify-content:center;align-items:center;height:100vh;margin:0">
<div style="text-align:center"><h1>Authorization successful</h1><p>You can close this window and return to opencode.</p>
<script>setTimeout(()=>window.close(),2000)</script></div></body></html>`

const renderOAuthError = (error: string) => `<!doctype html><html><head><title>Weave Router — Codex authorization failed</title></head>
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

// ---- Plugin --------------------------------------------------------------

export const WeaveCodex: Plugin = async (input: PluginInput): Promise<Hooks> => {
  return {
    auth: {
      provider: PROVIDER_ID,
      async loader(getAuth) {
        const auth = await getAuth()
        if (auth.type !== "oauth") return {}

        // Coalesce concurrent refreshes (opencode fires parallel turns).
        let refreshPromise: Promise<{ access: string; accountId: string | undefined }> | undefined

        return {
          apiKey: DUMMY_KEY,
          async fetch(requestInput: RequestInfo | URL, init?: RequestInit) {
            const currentAuth = (await getAuth()) as {
              type: string
              access: string
              refresh: string
              expires: number
              accountId?: string
            }
            if (currentAuth.type !== "oauth") return fetch(requestInput, init)

            // Refresh the access token on (or just before) expiry and persist
            // the rotated refresh token back into opencode's auth store.
            if (!currentAuth.access || currentAuth.expires < Date.now()) {
              if (!refreshPromise) {
                refreshPromise = refreshAccessToken(currentAuth.refresh)
                  .then(async (tokens) => {
                    const accountId = extractAccountId(tokens) || currentAuth.accountId
                    await input.client.auth.set({
                      path: { id: PROVIDER_ID },
                      body: {
                        type: "oauth",
                        // OAuth 2.0 lets the issuer omit a new refresh_token on
                        // refresh (the existing one stays valid). Keep the
                        // stored one in that case rather than clearing it.
                        refresh: tokens.refresh_token || currentAuth.refresh,
                        access: tokens.access_token,
                        expires: Date.now() + (tokens.expires_in ?? 3600) * 1000,
                        ...(accountId && { accountId }),
                      },
                    })
                    return { access: tokens.access_token, accountId }
                  })
                  .finally(() => {
                    refreshPromise = undefined
                  })
              }
              const refreshed = await refreshPromise
              currentAuth.access = refreshed.access
              currentAuth.accountId = refreshed.accountId
            }

            // Preserve the configured headers (X-Weave-Router-Key, X-App, ...
            // from opencode.json options.headers), then overwrite Authorization
            // with the caller's subscription bearer + the paired account id.
            const headers = new Headers(init?.headers as HeadersInit | undefined)
            headers.set("authorization", `Bearer ${currentAuth.access}`)
            if (currentAuth.accountId) headers.set("ChatGPT-Account-Id", currentAuth.accountId)

            // NOTE: unlike opencode's bundled codex plugin we deliberately do
            // NOT rewrite the URL — the request stays on the Weave Router, which
            // forwards it to the Codex backend on the caller's plan.
            return fetch(requestInput, { ...init, headers })
          },
        }
      },
      methods: [
        {
          label: "ChatGPT Pro/Plus (browser)",
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
          label: "ChatGPT Pro/Plus (headless device code)",
          type: "oauth",
          authorize: async () => {
            const deviceResponse = await fetch(`${ISSUER}/api/accounts/deviceauth/usercode`, {
              method: "POST",
              headers: { "Content-Type": "application/json", "User-Agent": USER_AGENT },
              body: JSON.stringify({ client_id: CLIENT_ID }),
            })
            if (!deviceResponse.ok) throw new Error("Failed to initiate device authorization")
            const deviceData = (await deviceResponse.json()) as {
              device_auth_id: string
              user_code: string
              interval: string
            }
            const interval = Math.max(parseInt(deviceData.interval) || 5, 1) * 1000
            return {
              url: `${ISSUER}/codex/device`,
              instructions: `Enter code: ${deviceData.user_code}`,
              method: "auto" as const,
              async callback() {
                const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
                while (true) {
                  const response = await fetch(`${ISSUER}/api/accounts/deviceauth/token`, {
                    method: "POST",
                    headers: { "Content-Type": "application/json", "User-Agent": USER_AGENT },
                    body: JSON.stringify({
                      device_auth_id: deviceData.device_auth_id,
                      user_code: deviceData.user_code,
                    }),
                  })
                  if (response.ok) {
                    const data = (await response.json()) as { authorization_code: string; code_verifier: string }
                    const tokenResponse = await fetch(`${ISSUER}/oauth/token`, {
                      method: "POST",
                      headers: { "Content-Type": "application/x-www-form-urlencoded" },
                      body: new URLSearchParams({
                        grant_type: "authorization_code",
                        code: data.authorization_code,
                        redirect_uri: `${ISSUER}/deviceauth/callback`,
                        client_id: CLIENT_ID,
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
    // The Codex backend (which the router forwards to) keys session continuity
    // off these headers; mirror opencode's bundled codex plugin. Scoped to our
    // provider so other providers are untouched.
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

export default WeaveCodex
