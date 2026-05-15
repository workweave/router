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

## Invariants

- **No I/O.** No HTTP, no DB, no filesystem.
- **No domain types.** Don't import `auth`, `proxy`, or anything from `internal/router/*`.
- **No provider package imports.** Translation must be addressable without pulling in `internal/providers/<name>`.
