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

The provider-backed `Summarizer` implementation for handover lives in [`handover.go`](handover.go); the inner-ring `handover` package only defines the contract. On summarizer timeout or error, proxy keeps the full prior history unchanged (it does **not** trim) — a pricier switch turn beats silently dropping the conversation the switched-to model needs.

## Translation

`proxy.Service` is the **only caller of [`../translate`](../translate)**. Keep providers ignorant of cross-format concerns. See [translate/CLAUDE.md](../translate/CLAUDE.md) for the recipe.

## Runtime provider fallback

Multi-binding models (deepseek/qwen/moonshot with Fireworks/DeepInfra/Bedrock primary + OpenRouter fallback in [`catalog.Model.Providers`](../router/catalog/catalog.go)) dispatch through [`dispatchWithFallback`](fallback.go). The helper walks the ordered binding list, retries on `providers.IsRetryable` errors (5xx/408/429 buffered responses, transport errors, `httputil.ErrUpstreamIdleTimeout`), and on exhaustion writes the final upstream error envelope via a format-specific renderer (`flushUpstreamErrorAsAnthropic` for ProxyMessages, `flushBufferedIfPresent` for ProxyOpenAIChatCompletion).

**Model-not-found 404 → cross-binding failover (only).** A buffered upstream 404 (`providers.IsUpstreamModelNotFound`) means the chosen provider doesn't serve the model — a stale/wrong upstream id or a provider with no active endpoints. It is deliberately *not* in `IsRetryable`: re-hitting the same provider is futile (so it never triggers same-binding retry), but a different provider binding may carry the model, so it triggers one cross-binding hop. This rescues a turn that would otherwise hard-fail at the client as "selected model may not exist." On the last binding the 404 still flushes.

**Single-binding same-binding retry.** Most catalog models carry one binding (Anthropic/OpenAI/Google), so cross-binding failover has nowhere to walk — a sole-provider 5xx/timeout would kill the turn. For these, `dispatchWithFallback` retries the *same* binding in place up to `maxSameBindingRetries` (2) with exponential backoff (`sameBindingBackoff`: 250ms, 500ms), pre-commit only, abortable on ctx cancel (`sleepWithContext`). Multi-binding models skip in-place retry (`len(bindings) > 1` breaks the inner loop) and fail straight over to the next provider — a different upstream beats re-hitting the flaky one. Tests inject `Service.retrySleep` to keep the backoff instant.

`preludeBuffer` wraps the client writer on every request so the eager SSE Prelude doesn't commit the response to the client before the upstream produces its first byte. The buffer absorbs pre-Seal writes (Prelude's status + `message_start`), commits on the first post-Seal write (= first upstream chunk), and `Discard()`s pre-commit state between attempts so a retry begins with a pristine writer. `Committed()` is the retry gate: once it flips, the response is on the wire and no further retry is allowed.

Per-attempt body rebuild: each closure constructs `EmitOptions` with `TargetProvider = d.Provider` so the OpenRouter-only gates in [`emit_openai.go`](../translate/emit_openai.go) (`provider` hint, `reasoning: {enabled:false}`, system reminder for tool turns, tool-temp override) fire on the OpenRouter attempt but not on Fireworks/etc. Otherwise OpenRouter would load-balance to non-DeepSeek-native hosts (no prefix caching) and reasoning would burn the max_tokens budget on hidden thinking.

**Invariants:**
- Unconditional wrap: `preludeBuffer` engages on every request (single- and multi-binding alike). The old single-binding bypass was removed after the v0.58 SWE-bench bake-off traced 46/84 empty-patch failures to it — bypassed requests shipped a marker-only turn to Claude Code on upstream api_errors. TTFB cost is ~200B of buffered SSE released the moment the upstream's first byte arrives. `Committed()` is the retry gate for both cross-binding failover and single-binding in-place retry.
- Retry gated on `preludeBuf.Committed() == false`. Once committed (first upstream byte flushed through the chain), switching providers mid-stream would interleave two model outputs.
- Per-attempt `Prepare*` + translator construction. Translators are stateful; a retry must rebuild the chain from scratch.
- BYOK and inbound-client-credential requests skip failover entirely (`shouldFailover()` returns false) — those keys bind to one provider and would 401 elsewhere.
- Cancel/deadline classified as non-retryable: client disconnect or per-request budget elapse must not waste a second upstream call.
- After dispatch, `actPricing` is re-resolved against the WINNING binding via `catalog.PriceFor(finalProvider, decision.Model)` so debits and OTel `cost.actual_*` reflect the actually-served provider's per-1M rate (the catalog's `PrimaryPriceFor` would otherwise always return the primary's).

## `OnUpstreamMeta` callbacks

Provider adapters call back into `proxy.OnUpstreamMeta` so streaming responses record usage/headers back to proxy without coupling provider packages to proxy internals. The pricing / planner stack depends on per-turn token counts being recorded promptly — **don't add a provider that forgets to call the callback.**

## What NOT to do

- **Don't move provider-call logic into planner.** Planner must remain pure so EV math is provable. Anything network-touching goes in `proxy.Service`.
- **Don't add a handover path that doesn't time out.** `Summarizer` contract says implementations MUST respect the context deadline. On timeout/error the proxy keeps the full prior history unchanged — do NOT reintroduce a silent trim-to-last-N fallback (it lobotomized switched-to models; see the handover-fallback fix).
- **Don't cache streaming responses.** Streaming bypasses cache on purpose — captured bytes would be post-translation SSE frames, and lookup latency budget is hostile to first-token-time. If you think this should change, write a doc first.
