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

## Scorer-unavailable degradation

When the cluster scorer returns `cluster.ErrClusterUnavailable` (embed timeout/error, dim mismatch, NaN argmax), `runTurnLoop` does not unconditionally 503. With `WithScorerFallback(true)` (composition root default: **on** for selfhosted, **off** for managed), `scorerFallbackDecision` (in [`turnloop.go`](turnloop.go)) serves the client's _explicitly requested_ model, resolved via `catalog.ResolveBinding` against the request's enabled-provider set. Intent-preserving (not a cheap default) and **loud, never silent**: ERROR log, `routing.degraded` OTel span attr, `x-router-degraded` response header, and a visibly degraded user-facing marker. The degraded turn skips the planner and is **not** written as a session pin (a transient blip must not become sticky). Scope: `ErrClusterUnavailable` only — `ErrNoEligibleProvider` is a config error the fallback can't fix (still 4xx); a requested model with no catalog binding still 503s. Why this doesn't reintroduce the masking the heuristic-fallback removal fixed: [`../router/cluster/CLAUDE.md`](../router/cluster/CLAUDE.md) → "Don't add fail-open fallbacks _in this package_".

## Translation

`proxy.Service` is the **only caller of [`../translate`](../translate)**. Keep providers ignorant of cross-format concerns. See [translate/CLAUDE.md](../translate/CLAUDE.md) for the recipe.

## `OnUpstreamMeta` callbacks

Provider adapters call back into `proxy.OnUpstreamMeta` so streaming responses record usage/headers back to proxy without coupling provider packages to proxy internals. The pricing / planner stack depends on per-turn token counts being recorded promptly — **don't add a provider that forgets to call the callback.**

## What NOT to do

- **Don't move provider-call logic into planner.** Planner must remain pure so EV math is provable. Anything network-touching goes in `proxy.Service`.
- **Don't add a handover path that doesn't time out.** `Summarizer` contract says implementations MUST respect the context deadline. Falling back to `TrimLastN` on timeout is correct, not a bug.
- **Don't cache streaming responses.** Streaming bypasses cache on purpose — captured bytes would be post-translation SSE frames, and lookup latency budget is hostile to first-token-time. If you think this should change, write a doc first.
- **Don't make the scorer-unavailable fallback silent or a fixed cheap default.** It must stay intent-preserving (the caller's requested model) and loud (ERROR log + `routing.degraded` span attr + `x-router-degraded` header + degraded marker). Silently serving a cheap default is exactly the regression-masking the heuristic-fallback removal fixed.
- **Don't pin or run the planner on a degraded turn.** A scorer-unavailable fallback must return before `writeNewPin`/planner — otherwise a transient blip becomes a sticky pin that contaminates later healthy turns for the session TTL.
