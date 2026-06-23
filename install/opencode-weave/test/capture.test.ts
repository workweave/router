/**
 * Capture tests for the Weave opencode subscription plugin (run under bun, the
 * runtime opencode itself uses): bun test install/opencode-weave/test
 *
 * These exercise the plugin's runtime contract without a live opencode:
 *   1. the `weave` loader attaches BOTH subscriptions to a request via the
 *      router's dedicated X-Weave-*-Subscription headers, preserves the
 *      configured router-key/identity headers, and leaves Authorization alone;
 *   2. an expired Claude token (read from the on-disk weave-claude slot) is
 *      refreshed and the rotated token persisted + injected;
 *   3. the `weave-claude` login hook builds the canonical Claude OAuth flow and
 *      exchanges a pasted code.
 *
 * Type-only imports from @opencode-ai/plugin are erased at runtime, so no
 * opencode package is needed to load the module.
 */
import { afterEach, beforeEach, describe, expect, test } from "bun:test"
import { mkdtemp, writeFile, readFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const ANTHROPIC_TOKEN_URL = "https://token.example.test/anthropic/oauth/token"
const CHATGPT_ISSUER = "https://issuer.example.test"

// Set the env overrides BEFORE importing the module (constants are read at load).
process.env.WEAVE_ANTHROPIC_OAUTH_TOKEN = ANTHROPIC_TOKEN_URL
process.env.WEAVE_CODEX_OAUTH_ISSUER = CHATGPT_ISSUER

const ROUTER_KEY = "rk_capture_test"
const CHATGPT_ACCESS = "eyJhbGciOiJChatGPTaccessJWT"
const CHATGPT_ACCOUNT = "acct-1234-5678"
const CLAUDE_ACCESS = "sk-ant-oat01-claude-access"
const CLAUDE_REFRESH = "claude-refresh-token"

const realFetch = globalThis.fetch
let authFile: string
let setCalls: Array<{ id: string; body: Record<string, unknown> }>

interface CapturedRequest {
  url: string
  headers: Record<string, string>
}

// Build a PluginInput whose client.auth.set records the write (and, to mimic
// persistence, rewrites the on-disk auth file the loader reads from).
function fakeInput() {
  return {
    client: {
      auth: {
        async set(opts: { path: { id: string }; body: Record<string, unknown> }) {
          setCalls.push({ id: opts.path.id, body: opts.body })
          const raw = JSON.parse(await readFile(authFile, "utf8"))
          raw[opts.path.id] = opts.body
          await writeFile(authFile, JSON.stringify(raw))
        },
      },
    },
  } as unknown as import("@opencode-ai/plugin").PluginInput
}

beforeEach(async () => {
  authFile = join(await mkdtemp(join(tmpdir(), "weave-auth-")), "auth.json")
  process.env.WEAVE_OPENCODE_AUTH_FILE = authFile
  setCalls = []
})

afterEach(() => {
  globalThis.fetch = realFetch
})

// Drive the loader's fetch once and capture the upstream request. tokenResponder
// answers any OAuth /oauth/token call (ChatGPT or Anthropic) so refreshes work.
async function runLoaderFetch(
  getAuth: () => Promise<Record<string, unknown>>,
  tokenResponder?: (url: string) => unknown,
): Promise<CapturedRequest> {
  const { WeaveCodex } = await import("../src/index.ts")
  const hooks = await WeaveCodex(fakeInput())
  const loaded = await hooks.auth!.loader!(getAuth as never, {} as never)

  let captured: CapturedRequest | undefined
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString()
    if (new URL(url).pathname.endsWith("/oauth/token")) {
      const body = tokenResponder ? tokenResponder(url) : {}
      // A responder returning null/undefined for a token URL simulates a failed
      // refresh (non-2xx), so tests can exercise the resilience paths.
      if (body === null || body === undefined) {
        return new Response("refresh failed", { status: 500 })
      }
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "Content-Type": "application/json" },
      })
    }
    const headers: Record<string, string> = {}
    new Headers(init?.headers as HeadersInit).forEach((v, k) => (headers[k] = v))
    captured = { url, headers }
    return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } })
  }) as typeof fetch

  // opencode's @ai-sdk provider sends the configured headers + an Authorization
  // bearer built from the (placeholder) apiKey.
  await (loaded.fetch as typeof fetch)("https://router.example.test/v1/responses", {
    method: "POST",
    headers: {
      "X-Weave-Router-Key": ROUTER_KEY,
      "X-App": "opencode",
      Authorization: "Bearer weave-router-oauth",
    },
  })
  if (!captured) throw new Error("upstream request was not captured")
  return captured
}

describe("weave loader — dual subscription injection", () => {
  test("attaches both subs via dedicated headers, preserves router key, leaves Authorization", async () => {
    // Claude slot present + unexpired on disk.
    await writeFile(
      authFile,
      JSON.stringify({
        "weave-claude": { type: "oauth", access: CLAUDE_ACCESS, refresh: CLAUDE_REFRESH, expires: Date.now() + 3_600_000 },
      }),
    )
    // ChatGPT (own provider) auth: unexpired.
    const getAuth = async () => ({
      type: "oauth",
      access: CHATGPT_ACCESS,
      refresh: "cg-refresh",
      expires: Date.now() + 3_600_000,
      accountId: CHATGPT_ACCOUNT,
    })

    const req = await runLoaderFetch(getAuth)

    expect(req.headers["x-weave-openai-subscription"]).toBe(CHATGPT_ACCESS)
    expect(req.headers["x-weave-openai-account-id"]).toBe(CHATGPT_ACCOUNT)
    expect(req.headers["x-weave-anthropic-subscription"]).toBe(CLAUDE_ACCESS)
    // Configured headers survive.
    expect(req.headers["x-weave-router-key"]).toBe(ROUTER_KEY)
    expect(req.headers["x-app"]).toBe("opencode")
    // Neither subscription leaks into Authorization (router resolves per model).
    expect(req.headers["authorization"]).not.toContain(CHATGPT_ACCESS)
    expect(req.headers["authorization"]).not.toContain(CLAUDE_ACCESS)
    // No refresh happened (tokens were fresh).
    expect(setCalls).toHaveLength(0)
  })

  test("ChatGPT-only (no Claude connected) attaches only the OpenAI sub", async () => {
    await writeFile(authFile, JSON.stringify({})) // no weave-claude slot
    const getAuth = async () => ({
      type: "oauth",
      access: CHATGPT_ACCESS,
      refresh: "cg-refresh",
      expires: Date.now() + 3_600_000,
      accountId: CHATGPT_ACCOUNT,
    })

    const req = await runLoaderFetch(getAuth)

    expect(req.headers["x-weave-openai-subscription"]).toBe(CHATGPT_ACCESS)
    expect(req.headers["x-weave-anthropic-subscription"]).toBeUndefined()
    expect(req.headers["x-weave-router-key"]).toBe(ROUTER_KEY)
  })

  test("a failed ChatGPT refresh still attaches the Claude sub (and doesn't fail the turn)", async () => {
    await writeFile(
      authFile,
      JSON.stringify({
        "weave-claude": { type: "oauth", access: CLAUDE_ACCESS, refresh: CLAUDE_REFRESH, expires: Date.now() + 3_600_000 },
      }),
    )
    // ChatGPT token is expired → triggers a refresh; the issuer returns 500.
    const getAuth = async () => ({
      type: "oauth",
      access: "stale",
      refresh: "cg-refresh",
      expires: Date.now() - 1000,
      accountId: CHATGPT_ACCOUNT,
    })

    const req = await runLoaderFetch(getAuth, (url) => (url === `${CHATGPT_ISSUER}/oauth/token` ? undefined : {}))

    // ChatGPT refresh failed → no OpenAI headers, but the turn still went out…
    expect(req.headers["x-weave-openai-subscription"]).toBeUndefined()
    // …with the (unaffected) Claude sub attached.
    expect(req.headers["x-weave-anthropic-subscription"]).toBe(CLAUDE_ACCESS)
    expect(req.headers["x-weave-router-key"]).toBe(ROUTER_KEY)
  })

  test("does not inject an expired Claude token that has no refresh token", async () => {
    await writeFile(
      authFile,
      JSON.stringify({
        "weave-claude": { type: "oauth", access: "expired-claude", expires: Date.now() - 1000 },
      }),
    )
    const getAuth = async () => ({
      type: "oauth",
      access: CHATGPT_ACCESS,
      refresh: "cg-refresh",
      expires: Date.now() + 3_600_000,
      accountId: CHATGPT_ACCOUNT,
    })

    const req = await runLoaderFetch(getAuth)

    // Dead Claude sub (expired, unrefreshable) is dropped so the router bills the
    // Weave key rather than treating it as present.
    expect(req.headers["x-weave-anthropic-subscription"]).toBeUndefined()
    expect(req.headers["x-weave-openai-subscription"]).toBe(CHATGPT_ACCESS)
  })

  test("refreshes an expired Claude token, persists it to weave-claude, and injects the rotated token", async () => {
    await writeFile(
      authFile,
      JSON.stringify({
        "weave-claude": { type: "oauth", access: "stale", refresh: CLAUDE_REFRESH, expires: Date.now() - 1000 },
      }),
    )
    const getAuth = async () => ({
      type: "oauth",
      access: CHATGPT_ACCESS,
      refresh: "cg-refresh",
      expires: Date.now() + 3_600_000,
      accountId: CHATGPT_ACCOUNT,
    })

    const rotated = "sk-ant-oat01-rotated"
    const req = await runLoaderFetch(getAuth, (url) => {
      if (url === ANTHROPIC_TOKEN_URL) {
        return { access_token: rotated, refresh_token: "claude-refresh-2", expires_in: 3600 }
      }
      return {}
    })

    // Rotated token injected on the wire.
    expect(req.headers["x-weave-anthropic-subscription"]).toBe(rotated)
    // Persisted back to the weave-claude slot.
    const claudeSet = setCalls.find((c) => c.id === "weave-claude")
    expect(claudeSet).toBeDefined()
    expect(claudeSet!.body.access).toBe(rotated)
    expect(claudeSet!.body.refresh).toBe("claude-refresh-2")
  })

  test("loader is inert when the provider has no oauth (no subs attached)", async () => {
    await writeFile(authFile, JSON.stringify({}))
    const { WeaveCodex } = await import("../src/index.ts")
    const hooks = await WeaveCodex(fakeInput())
    const loaded = await hooks.auth!.loader!(
      (async () => ({ type: "api", key: "x" })) as never,
      {} as never,
    )
    expect(Object.keys(loaded)).toHaveLength(0) // {} — opencode falls back to static config
  })
})

describe("weave-claude login hook", () => {
  test("exposes the Claude login on its own provider with the canonical OAuth flow", async () => {
    const { WeaveClaude } = await import("../src/index.ts")
    const hooks = await WeaveClaude(fakeInput())
    expect(hooks.auth!.provider).toBe("weave-claude")
    const method = hooks.auth!.methods[0]
    expect(method.type).toBe("oauth")
    expect(method.label).toContain("Claude")

    const result = await (method as { authorize: () => Promise<{ url: string; method: string }> }).authorize()
    const url = new URL(result.url)
    expect(url.origin).toBe("https://claude.ai")
    expect(url.pathname).toBe("/oauth/authorize")
    expect(url.searchParams.get("client_id")).toBe("9d1c250a-e61b-44d9-88ed-5944d1962f5e")
    expect(url.searchParams.get("redirect_uri")).toBe("https://console.anthropic.com/oauth/code/callback")
    expect(url.searchParams.get("code_challenge_method")).toBe("S256")
    expect(result.method).toBe("code")
  })

  test("rejects a pasted code missing the #state separator with a clear error", async () => {
    const { WeaveClaude } = await import("../src/index.ts")
    const hooks = await WeaveClaude(fakeInput())
    const method = hooks.auth!.methods[0] as {
      authorize: () => Promise<{ callback: (code: string) => Promise<Record<string, unknown>> }>
    }
    const flow = await method.authorize()
    await expect(flow.callback("JUSTACODE")).rejects.toThrow(/code#state/)
  })

  test("exchanges a pasted code#state for tokens", async () => {
    const { WeaveClaude } = await import("../src/index.ts")
    const hooks = await WeaveClaude(fakeInput())
    const method = hooks.auth!.methods[0] as {
      authorize: () => Promise<{ callback: (code: string) => Promise<Record<string, unknown>> }>
    }
    const flow = await method.authorize()

    let exchangeBody: Record<string, unknown> | undefined
    globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
      exchangeBody = JSON.parse(String(init?.body))
      return new Response(
        JSON.stringify({ access_token: CLAUDE_ACCESS, refresh_token: CLAUDE_REFRESH, expires_in: 3600 }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      )
    }) as typeof fetch

    const out = await flow.callback("THECODE#THESTATE")
    expect(out.type).toBe("success")
    expect(out.access).toBe(CLAUDE_ACCESS)
    expect(out.refresh).toBe(CLAUDE_REFRESH)
    // Code/state split on the "#" the manual flow returns.
    expect(exchangeBody!.code).toBe("THECODE")
    expect(exchangeBody!.state).toBe("THESTATE")
    expect(exchangeBody!.grant_type).toBe("authorization_code")
  })
})

describe("opencode plugin-loading contract", () => {
  // opencode's loader (applyPlugin → readV1Plugin(mod, _, "server", "detect"))
  // treats a module whose `default` is a plain OBJECT with id/server/tui as a V1
  // module and loads ONLY `default.server`. Only when `default` is NOT such an
  // object does it fall back to getLegacyPlugins → Object.values(mod), loading
  // EVERY exported Plugin function. We rely on that fallback so BOTH WeaveCodex
  // (provider `weave`) and WeaveClaude (provider `weave-claude`) register from one
  // file. These assertions fail if someone converts the module to a `{ server }`
  // default export (which would silently drop the named Claude-login plugin).
  test("default export is a bare function (takes the all-exports legacy path)", async () => {
    const mod = await import("../src/index.ts")
    expect(typeof mod.default).toBe("function")
    // A function is not a record, so readV1Plugin returns undefined in detect mode.
    expect(mod.default).not.toBeNull()
    expect(["id", "server", "tui"].some((k) => k in (mod.default as object))).toBe(false)
  })

  test("both Plugins are exported functions that opencode's Object.values loop will load", async () => {
    const mod = (await import("../src/index.ts")) as unknown as Record<string, unknown>
    const plugins = Object.values(mod).filter((v) => typeof v === "function")
    // Dedup by reference (default === WeaveCodex), as opencode's loader does.
    const unique = new Set(plugins)
    const providers = new Set<string>()
    for (const p of unique) {
      const hooks = await (p as (i: unknown) => Promise<{ auth?: { provider: string } }>)(fakeInput())
      if (hooks.auth) providers.add(hooks.auth.provider)
    }
    expect(providers.has("weave")).toBe(true)
    expect(providers.has("weave-claude")).toBe(true)
  })
})
