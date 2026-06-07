# Plan: show the Weave Router badge on the native Anthropic (Opus) path

## Problem

The `✦ **Weave Router** → <model>` badge never appears when the router picks an
Anthropic model (e.g. Opus). DeepSeek/Qwen/Gemini show it; Opus does not, so it
looks like the router is doing nothing on the one path users notice most.

### Why

The badge is injected by a same-format marker writer wrapped around the client
sink. The OpenAI and Gemini **native passthrough** surfaces already have one:

- `OpenAIRoutingMarkerWriter` — `internal/translate/openai_marker.go`
- `GeminiRoutingMarkerWriter` — `internal/translate/gemini_marker.go`

Both are wired via the `makeMarkerSink()` closure in `internal/proxy/service.go`
(around line 2004, in `ProxyOpenAIChatCompletion`).

There is **no `AnthropicRoutingMarkerWriter`**. In `ProxyMessages`, the
`providers.ProviderAnthropic` branch (`service.go` ~1064–1081) is a raw
`PrepareAnthropic` → `p.Proxy(...) → sink` byte copy with nothing wrapping the
sink, so no marker is ever emitted. That is the entire reason Opus has no badge.

### Why NOT the literal "reuse AnthropicSSETranslator" idea

`AnthropicSSETranslator` (`stream.go:326`) parses **OpenAI** SSE → Anthropic. It
is not an identity pass. Reusing it on the native branch means a double
translation: Anthropic upstream → OpenAI (`NewSSETranslator`) → Anthropic
(`NewAnthropicSSETranslator`). That round-trip:

1. **Drops thinking blocks.** The Anthropic→OpenAI leg
   (`handleContentBlockDelta`, `stream.go:228`) only handles `text_delta` and
   `input_json_delta`; `thinking_delta` / `signature_delta` fall through
   `default: return nil`. Opus streams thinking + signatures, so the round-trip
   silently strips them — a real regression on the highest-value path.
2. **Is not actually less special-cased** — it's a bespoke two-stage chain no
   other native path uses.

So the genuinely symmetric fix is to add an `AnthropicRoutingMarkerWriter` peer
of the existing two writers and wire the Anthropic branch through the same
`makeMarkerSink()` pattern. All three native paths become symmetric, and Opus
thinking blocks are preserved.

### The one extra wrinkle vs. OpenAI/Gemini

Anthropic SSE carries an explicit `index` on every `content_block_*` event.
Prepending the marker as a text block at index 0 means every subsequent upstream
block's `index` must shift by +1. (OpenAI/Gemini concatenate deltas and need no
renumbering, which is why their writers are simpler.)

## Changes

### 1. New file: `internal/translate/anthropic_marker.go`

`AnthropicRoutingMarkerWriter` — a same-format SSE writer wrapping the sink,
sibling to `OpenAIRoutingMarkerWriter`.

- `NewAnthropicRoutingMarkerWriter(w http.ResponseWriter, model, marker string)`.
- `Prelude(streaming bool) error`: commit `text/event-stream` + HTTP 200, emit
  our own `message_start` (reuse the envelope shape from
  `AnthropicSSETranslator.emitMessageStart`, `stream.go:1164`), then the marker
  as `content_block_start` / `content_block_delta` (text_delta) /
  `content_block_stop` at **index 0**. Mirrors the translator's `Prelude`
  (`stream.go:574`) + `emitRoutingMarkerIfConfigured` (`stream.go:1098`).
- `Write(data []byte) error`: parse upstream SSE with `sse.SplitNext` /
  `sse.ParseEvent`; then:
  - **drop** the upstream's `message_start` (we already sent one);
  - for every `content_block_start` / `content_block_delta` /
    `content_block_stop`, rewrite `index` → `index + 1` via `sjson`;
  - pass `message_delta` / `message_stop` / `ping` / `error` through untouched.
- Empty marker **or** non-streaming → fully transparent passthrough (same
  convention the OpenAI/Gemini writers use; non-streaming stays unmarked on all
  three surfaces).
- `Header()` / `WriteHeader()` / `Flush()` plus compile-time asserts
  `var _ http.ResponseWriter` and `var _ http.Flusher`.

### 2. Wire into `ProxyMessages` — `internal/proxy/service.go` (~1064–1081)

- Add a `makeMarkerSink()` closure mirroring `service.go:2004`:
  `marker := suppressMarkerIfRequested(r.Header, routingMarkerFor(routeRes))`.
- In the `providers.ProviderAnthropic` attempt closure, wrap `sink` with
  `NewAnthropicRoutingMarkerWriter`, call `Prelude(env.Stream())` **before**
  `preludeBuf.Seal()`, and on post-commit streaming error emit via
  `emitAnthropicSSEErrorEvent` (already used at `service.go:1124`).
- Keep `UsageExtractor` **outermost** so token accounting reads real upstream
  usage. The marker writer only touches `content_block_*` indices, never
  `message_start` / `message_delta` usage fields — confirmed against
  `otel/usage.go:163` (extractor reads only `message_start` / `message_delta`).

### 3. Tests: `internal/translate/anthropic_marker_test.go`

Mirror `openai_marker_test.go`:

- Streaming injects exactly one marker text block at index 0; a tool_use
  originally at index 1 surfaces at index 2.
- **Thinking fidelity**: a `thinking_delta` + `signature_delta` stream passes
  through intact with shifted index (the regression the round-trip would cause).
- Empty marker / `X-Weave-Routing-Marker: off` → byte-identical passthrough.
- Non-streaming → untouched.
- Upstream `message_start` suppressed; exactly one `message_start` reaches the
  client.

### 4. Non-streaming: leave unmarked

OpenAI/Gemini native paths don't badge non-streaming responses (their writers
pass through), so for symmetry the Anthropic path does the same. No change to
the non-streaming JSON path.

## Validation

- `wv mr tc` and `wv mr t` (run in `router-internal/router` / this repo).
- `testing-router-locally` skill: confirm a live Opus turn shows the badge with
  thinking blocks intact.
- Router-repo PR (these changes live in the router submodule), then bump the
  WorkWeave gitlink (`wv mr sync` + commit).

## Open decisions (resolved)

1. **Reframe accepted**: build `AnthropicRoutingMarkerWriter` (symmetric with
   OpenAI/Gemini, preserves thinking) rather than the literal round-trip — the
   round-trip drops Opus thinking blocks.
2. **Index-shift accepted**: faithful `index + 1` rewrite so the marker at index
   0 never collides with the upstream's first block (avoids Claude Code
   overwriting / mis-rendering the badge).
