# internal/proxy — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Routing/dispatch service. Per-turn orchestrator that composes scorer, planner, handover, cache, sessionpin, pricing, capability, turntype, usage, providers, and translate. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Surface

`*proxy.Service` in [`service.go`](service.go) exposes:

- `Route` — returns `router.Decision`
- `ProxyMessages` — Anthropic Messages surface
- `ProxyOpenAIChatCompletion` — OpenAI Chat Completions
- `ProxyGemini` — Gemini native generateContent

Plus the turn loop, handover adapter, cache writer, and session-key derivation in sibling files.

## Adding a method to `*proxy.Service`

1. **Define method on `*proxy.Service`.** No I/O directly here — push into a provider adapter or repo. Inner-ring imports (`router`, `providers`, `translate`, `observability`, `internal/router/*`, `internal/proxy/usage`) + small utility libs are fine.
2. **If you need new repo methods**, surface them as an interface in the inner-ring package, implement in `internal/postgres/`. Example: `sessionpin.Store` in [`../router/sessionpin/store.go`](../router/sessionpin/store.go), implemented by `postgres.SessionPinRepository`.
3. **Update `service_test.go` fakes** to satisfy the expanded interface. Real assertions on return values, not "mock called with X".

## Per-turn flow (cache-aware turn routing)

The per-turn flow is more than "scorer → dispatch". Pinned session, planner verdict, optional handover summary, and the semantic response cache all sit between the inbound request and the upstream provider. Packages are intentionally small + single-purpose so each is unit-testable without the others.

| Step | Package | Notes |
|---|---|---|
| Turn-type classification | [`../router/turntype`](../router/turntype) | MainLoop / ToolResult / SubAgentDispatch / Compaction / Probe |
| Session-pin lookup | [`../router/sessionpin`](../router/sessionpin) | Sticky `(api_key_id, session_key, role)` pin |
| Fresh routing decision | [`../router/cluster`](../router/cluster) | Cluster scorer argmax |
| STAY vs SWITCH | [`../router/planner`](../router/planner) | Cache-aware EV policy |
| Handover summary on SWITCH | [`../router/handover`](../router/handover) | Bounds switch-turn input cost |
| Semantic response cache | [`../router/cache`](../router/cache) | Cross-request, non-streaming only |
| Anthropic usage-bypass gate | [`usage`](usage) | See [`usage/CLAUDE.md`](usage/CLAUDE.md) |

The provider-backed `Summarizer` implementation for handover lives in [`handover.go`](handover.go); the inner-ring `handover` package only defines the contract. On summarizer timeout or error, proxy falls back to `handover.TrimLastN`.

## Translation

`proxy.Service` is the **only caller of [`../translate`](../translate)**. Keep providers ignorant of cross-format concerns. See [translate/CLAUDE.md](../translate/CLAUDE.md) for the recipe.

## `OnUpstreamMeta` callbacks

Provider adapters call back into `proxy.OnUpstreamMeta` so streaming responses record usage/headers back to proxy without coupling provider packages to proxy internals. The pricing / planner stack depends on per-turn token counts being recorded promptly — **don't add a provider that forgets to call the callback.**

## What NOT to do

- **Don't move provider-call logic into planner.** Planner must remain pure so EV math is provable. Anything network-touching goes in `proxy.Service`.
- **Don't add a handover path that doesn't time out.** `Summarizer` contract says implementations MUST respect the context deadline. Falling back to `TrimLastN` on timeout is correct, not a bug.
- **Don't cache streaming responses.** Streaming bypasses cache on purpose — captured bytes would be post-translation SSE frames, and lookup latency budget is hostile to first-token-time. If you think this should change, write a doc first.
