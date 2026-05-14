Created: 2026-05-13
Last edited: 2026-05-13

# Router OpenRouter caching — follow-up fixes

## Context

On 2026-05-13 we audited a slow Claude Code session routed through the
WorkWeave router. Findings:

- `route_ms` was 0–300 ms across 279 requests — the router itself is not
  the bottleneck.
- DeepSeek v4-pro requests via OpenRouter averaged **12.2 s** with a tail
  to **71 s**. Moonshot Kimi k2.5 averaged 19.4 s.
- The OpenRouter activity CSV showed `tokens_cached=0` for every
  DeepSeek call (TTFT ~22 s on 96K-token prompts) — confirming zero
  prefix-cache hits.
- Without a `provider` field on the OpenRouter request, OpenRouter
  load-balances by price across all hosts serving the model. DeepSeek
  traffic was landing on **Parasail** and **Baidu**, neither of which
  implements prefix caching.
- The router's translate layer drops `cache_control` markers when
  emitting OpenAI-format bodies, per the (now-stale) rule in
  [`CLAUDE.md`](../CLAUDE.md) / [`AGENTS.md`](../AGENTS.md) line 603.

**Fix 1** — pin DeepSeek and Moonshot to their native OpenRouter
endpoints by injecting a `provider` hint — landed in
[`internal/translate/provider_hint.go`](../internal/translate/provider_hint.go)
and the two `emit_openai.go` build paths. Tests in
[`provider_hint_test.go`](../internal/translate/provider_hint_test.go).

This doc tracks the remaining two fixes that came out of the audit.

## Fix 2 — Preserve `cache_control` on the OpenAI emit path

### Why

DeepSeek and Moonshot do prefix caching automatically (no `cache_control`
needed), so Fix 1 alone should drive TTFT down on continuation turns.
But OpenRouter forwards `cache_control` for several providers that
**require** it:

| Provider (via OpenRouter) | Automatic caching? | Needs `cache_control`? |
|---|---|---|
| Anthropic | no | **yes** |
| DeepSeek | yes | no |
| Moonshot/Kimi | yes | no |
| Gemini 2.5 | yes (implicit) | no |
| OpenAI | yes | no |
| Qwen | no | **yes** |

So the moment we route to Anthropic-via-OpenRouter, Qwen, or want to use
Anthropic's 1-hour TTL (`{"type":"ephemeral","ttl":"1h"}`) on direct
Anthropic calls, the current translate layer breaks caching by stripping
the markers.

### What the code does today

[`internal/translate/emit_openai.go`](../internal/translate/emit_openai.go)
drops `cache_control` at every leaf when going Anthropic → OpenAI:

- `flattenAnthropicSystem` (lines 91–118) joins all system text blocks
  with `\n` into a single string; per-block `cache_control` is lost.
- `anthropicUserToOpenAI` (188–230) copies `{type:"text", text:…}` parts
  but skips `cache_control`.
- `anthropicAssistantToOpenAI` (141–186) joins assistant text into one
  string; same loss.
- `pullAnthropicTools` (289–318) builds new tool objects with
  `name`/`description`/`parameters` only; tool-def `cache_control` is
  dropped.

A `grep -rn cache_control --include='*.go' router-internal/router/`
confirms zero references anywhere in router Go code.

### What to change

Add a `target Target` enum to `EmitOptions` (or a `TargetIsOpenRouter
bool` — pick whichever fits the existing style) so the emit functions
know whether `cache_control` should be preserved. First-party OpenAI
and Google don't recognize the field and ignore it harmlessly, but
keeping the wire shape clean is worthwhile.

Then modify the four functions above:

1. **`flattenAnthropicSystem`** — when any block in the input array
   has `cache_control`, emit `{role: "system", content: [...blocks
   with cache_control preserved...]}` (OpenAI/OpenRouter accept array
   content on the system role). When no block has the marker, keep
   the existing single-string output.

2. **`anthropicUserToOpenAI`** — at lines 204 and 209 where we build
   the `userParts` map, copy `block["cache_control"]` onto the OpenAI
   part when present:
   ```go
   part := map[string]any{"type": "text", "text": block["text"]}
   if cc, ok := block["cache_control"]; ok {
       part["cache_control"] = cc
   }
   ```
   Skip the "collapse single text part to string content" shortcut
   (lines 215–221) when `cache_control` is set, otherwise the marker
   is lost again.

3. **`anthropicAssistantToOpenAI`** — same treatment as user. When any
   block has `cache_control`, emit array-form content instead of
   `strings.Join`.

4. **`pullAnthropicTools`** — at line 311 where we build the tool
   wrapper, copy `cache_control` from the source Anthropic tool onto
   the wrapper:
   ```go
   entry := map[string]any{"type": "function", "function": fn}
   if cc, ok := tool["cache_control"]; ok {
       entry["cache_control"] = cc
   }
   result = append(result, entry)
   ```

### Update the docs

Flip the rule in [`CLAUDE.md`](../CLAUDE.md) and
[`AGENTS.md`](../AGENTS.md) at line 603 from:

> Don't leak Anthropic-only fields (`thinking`, `cache_control`, `metadata`) when targeting OpenAI/Google.

to something like:

> When emitting OpenAI format, preserve `cache_control` for OpenRouter
> targets (DeepSeek, Moonshot, Gemini 2.5, Anthropic, Qwen forward it
> to the provider) and strip it for first-party OpenAI and Google.
> Always strip `thinking` and `metadata` — those remain Anthropic-only.

### Tests

Add `cache_control_preserve_test.go` next to
`provider_hint_test.go`. Cover:

- system block with `cache_control` survives Anthropic → OpenAI emit
  (output uses array-form content).
- user-message text block with `cache_control` survives, including
  the single-block-no-collapse case.
- assistant-message text block with `cache_control` survives.
- tool definition with `cache_control` produces a top-level
  `cache_control` on the OpenAI tool entry.
- target = first-party OpenAI: markers stripped.
- target = OpenRouter: markers preserved.

### Verification

After landing, write an end-to-end test (or a curl-and-eyeball check
via `wv mr`) that sends a request through `/v1/messages` with
`cache_control` on the system prompt, captures the outbound OpenAI
body via a tcp proxy or log instrumentation, and asserts the field
survived.

## Fix 3 — Capture upstream cache stats in telemetry

### Why

We currently log `ProxyMessages complete` with `upstream_status` and
timing, but nothing about token usage. Cache-hit rate is invisible
to us until someone pulls the OpenRouter activity CSV out-of-band —
which means we can't tell at a glance whether Fix 1 and Fix 2 are
actually working in production.

The `upstream_status=0` we saw on all 279 audited requests is a
separate (cosmetic) issue: `upstreamStatus(proxyErr)` only returns
non-zero when `proxyErr` is a `*providers.UpstreamStatusError`. On
2xx responses `proxyErr` is nil and the field reads `0`. Either
rename the field, or wire the real status code through and log it
always.

### What the code does today

[`internal/proxy/service.go:1071`](../internal/proxy/service.go) emits
the structured log line. The fields come from inputs and routing
state — nothing is read from the upstream response body. The OpenAI-
compat adapter at
[`internal/providers/openaicompat/client.go:116`](../internal/providers/openaicompat/client.go)
streams the response straight through via `httputil.StreamBody`
without parsing anything.

### What to change

1. **Tee the final SSE `data:` chunk** that contains the `usage`
   object through a small parser. The natural home is in
   `internal/sse/` or via an interceptor decorator around
   `httputil.StreamBody`. The OpenAI streaming format puts the
   terminal usage in the last `data: {…}` event when
   `stream_options.include_usage=true` (which the router already
   injects — see `injectStreamUsageOption` in
   [`emit_openai.go`](../internal/translate/emit_openai.go)).

2. **Stash on the request context.** A small struct held in a
   `context.Context` value:
   ```go
   type UpstreamUsage struct {
       PromptTokens     int64
       CompletionTokens int64
       CachedTokens     int64 // usage.prompt_tokens_details.cached_tokens
       CacheWriteTokens int64
       UpstreamProvider string // OpenRouter's "provider" response field
   }
   ```
   Set it from the SSE interceptor; read it at the
   `ProxyMessages complete` log site.

3. **Extend the log line** at
   [`service.go:1071`](../internal/proxy/service.go) with:
   `upstream_provider`, `prompt_tokens`, `cached_tokens`,
   `completion_tokens`, `cache_hit_ratio`
   (= `cached_tokens / prompt_tokens` to 2 d.p., or omit when
   `prompt_tokens=0`).

4. **Fix `upstream_status=0` on success.** Either:
   - log `upstream_status` only on errors (drop it on success), or
   - thread the real status (`resp.StatusCode`) from the openaicompat
     adapter through the context the same way as upstream usage.

   The latter is more useful in dashboards but is more wiring.
   Recommend the former for v1.

### Provider name capture

OpenRouter includes the upstream provider in the response as
`"provider": "DeepSeek"` (top level, not under `usage`). The SSE
interceptor should pluck this from the first chunk that carries
the field. If you log this every request, you can confirm Fix 1 is
working without going to the OpenRouter dashboard.

### Tests

- Unit test the SSE interceptor with a recorded DeepSeek stream
  (final chunk has usage with `prompt_tokens_details.cached_tokens`);
  assert the parsed `UpstreamUsage` matches.
- Unit test the log line wiring with a fake provider client that
  populates the context.

### Verification

Send 5 sequential Claude Code turns through `wv mr` against a
DeepSeek target. Tail router logs. Expect:
- Turn 1: `cached_tokens=0`, `upstream_provider=DeepSeek`.
- Turns 2–5: `cached_tokens` close to `prompt_tokens` minus the
  trailing user message; `cache_hit_ratio > 0.9`.

## Open questions

1. **Verify the OpenRouter provider slugs** before shipping Fix 1 to
   prod. The fix uses `"DeepSeek"` and `"Moonshot AI"`. Hit
   `https://openrouter.ai/api/v1/models/deepseek/deepseek-v4-pro/endpoints`
   with an OpenRouter API key and confirm the `provider_name` strings
   match. If they don't, update
   [`provider_hint.go`](../internal/translate/provider_hint.go).

2. **Should the provider hint be configurable per deployment?**
   Today it's hard-coded by model-slug prefix. Reasonable for v1.
   If we later need per-org overrides (e.g. a customer with a BYOK
   key for a specific Together endpoint), surface it through the
   existing config layer.

3. **What about Anthropic models routed through OpenRouter?** We
   currently send those direct via the first-party Anthropic
   adapter, so Fix 1 doesn't apply. If we ever route Anthropic
   through OpenRouter (e.g. for BYOK proxies), Fix 2 becomes
   load-bearing — Anthropic requires `cache_control` markers and
   has no automatic mode.
