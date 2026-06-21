# internal/translate — CLAUDE

> **Mirror notice.** Verbatim sync with [AGENTS.md](AGENTS.md). **Update both together** — divergence = bug.

Cross-format wire-format conversion. Pure functions, no I/O, no provider knowledge, no domain types. Read [root CLAUDE.md](../../CLAUDE.md) first.

## Scope

Covers all three directions: Anthropic ⇄ OpenAI and Gemini ⇄ {Anthropic, OpenAI} via a `RequestEnvelope` intermediate + per-target `emit_*.go` files.

**Only [`../proxy`](../proxy) calls this package.** Providers stay ignorant of cross-format concerns.

## Adding a wire-format pair

When a new inbound format needs to talk to an existing upstream provider with a different wire format:

1. **Add conversion functions to this package.** Pure functions only — no I/O, no provider knowledge, no domain types.
2. **If response streaming, adapt [`stream.go`](stream.go) / [`gemini_stream.go`](gemini_stream.go)** or add a sibling decorator. Decorators wrap `http.ResponseWriter` and translate on the fly so we never buffer entire responses. Use [`../sse`](../sse) for zero-alloc SSE framing.
3. **Compose the new translation in `proxy.Service.Proxy*`.** Proxy is the only caller of `translate`.

## Anthropic-specific stripping (load-bearing)

Anthropic-only fields (`thinking`, `cache_control`, `metadata`, Anthropic beta headers) are stripped at translation time **and again defensively in the OpenAI / openaicompat adapters**. Keep both checks — belt-and-suspenders is intentional because the field set drifts as Anthropic adds beta features.

## Gemini 3.x `thoughtSignature` (load-bearing)

The router translator must **round-trip `thoughtSignature` on text / thinking blocks as well as `functionCall` blocks**. Dropping it on text parts breaks the next turn against Gemini 3.x preview models with a 400. The native Generative Language REST client in [`../providers/google`](../providers/google) is mandatory for those flows; the OpenAI-compat surface at `/v1beta/openai` does **not** preserve `thoughtSignature`.

**Carrier: the tool id, not an off-spec field.** For `tool_use` / `functionCall` blocks the signature is the **single** carrier smuggled into the block's id ([`thought_signature_id.go`](thought_signature_id.go), `__thought__<base64>`) — a typed string every client SDK round-trips. Do **not** also emit a raw `thought_signature` block field for tool calls: typed SDKs drop it, and any client that *does* echo it back (Claude Code) makes the next turn 400 if it re-routes to Anthropic (`tool_use.thought_signature: Extra inputs are not permitted`). Text blocks have no id, so they keep the raw field as their only carrier. Targeting Anthropic, `resolveAnthropicOverrides` strips the raw field from **all** blocks (`StripThoughtSignature`) — lossless for tool calls (id still carries it), and the only safe option for text (Anthropic can't use a Gemini signature). The OpenAI emit paths clamp the now-oversized id back under OpenAI's 64-char `call_id`/`tool_calls[].id` limit (`clampOpenAIToolCallID`).

## Tool-call validation + strict decoding (load-bearing)

Model-emitted tool_use arguments are validated against the inbound request's `tools[].input_schema` by [`toolcheck`](toolcheck/) at every response-translation point (OpenAI-compat and Gemini chains, streaming + non-streaming, and both Responses paths). The pipeline: normalize (drop empty-string/null OPTIONAL params) -> minimal JSON repair -> Draft-7 validation -> safe deterministic repair (drop unknown keys, lossless coercions), re-validated. Unrepairable schema mismatches forward as-emitted (the client's own tool error is the feedback loop); only unparseable JSON degrades to `{}`. Every finding surfaces via `ResponseSummary.ToolCallIssues`, which the proxy logs as `router.tool_call_invalid`. **Everything is fail-open** — a schema that won't compile must never fail a request.

On the emit side the failure class is prevented at decode time where the upstream exposes a knob: OpenAI Responses tools go out with `strict:true` + a strictified schema ([`strictify_openai.go`](strictify_openai.go) — additionalProperties:false, all-required, optionals as null unions; non-strictifiable schemas fall back to non-strict). Gemini 3.x gets `functionCallingConfig.mode=VALIDATED` when the client didn't force a tool_choice. Proxy-side validation always checks against the ORIGINAL schema — the explicit nulls strict mode induces are dropped by toolcheck's normalize pass.

## Invariants

- **No I/O.** No HTTP, no DB, no filesystem.
- **No domain types.** Don't import `auth`, `proxy`, or anything from `internal/router/*`.
- **No provider package imports.** Translation must be addressable without pulling in `internal/providers/<name>`.
