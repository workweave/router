# Plan: Convert MiMo `<think>` blocks to Anthropic thinking

## Problem
`xiaomi/mimo-v2.5-pro` (catalog.go:366; DeepInfra primary + OpenRouter fallback via `openaicompat` chat-completions) streams chain-of-thought as inline `<think>…</think>` in the **`content`** channel — not in `reasoning_content`/`reasoning` (which the translator already routes to Anthropic `thinking`: stream.go:803-829, translate.go:235-246). So Claude Code renders raw `<think>…</think>` as visible prose.

Today this is left as text on purpose — see load-bearing comment `stream.go:1024-1032` (excludes `<think>` from `toolishMarkupMarkers`). This plan supersedes that: reroute a **leading** `<think>…</think>` from content into a `thinking` block.

## Affected paths (Anthropic `/v1/messages` → MiMo, OpenAI-compat upstream)
- **Streaming**: `AnthropicSSETranslator.emitDelta` content branch, `stream.go:831-872` (hard: tags split across deltas).
- **Non-streaming**: `writeAnthropicContentFromOpenAI`, `translate.go:233-254`.

Out of scope (MiMo never traverses): Responses→Anthropic writer (`responses_to_anthropic_writer.go`); OpenAI/Gemini-inbound passthrough.

## Principle: anchor to start
Only treat `<think>` as reasoning when content **opens** with it (after leading whitespace), mirroring `leadsWithToolishMarkup` (stream.go:1035-1047). Mid-prose `<think>` must NOT trigger.

---

## Phase 1 — Pure splitter (`internal/translate/think_tag.go`)
Stateful scanner, no I/O, unit-testable:

```go
type thinkTagSplitter struct { state thinkState; pending strings.Builder }
type thinkSegment struct { kind thinkKind; text string } // segThinking | segText
func (s *thinkTagSplitter) Feed(content string) []thinkSegment
func (s *thinkTagSplitter) Flush() []thinkSegment // unclosed <think> → thinking
```
States: `scanningOpen` → buffer; skip leading ws; if next non-ws run is a prefix of `<think>`, hold in `pending`; full `<think>` → `insideThink`; definitive non-match → emit buffer as text, go `passthrough` permanently. `insideThink` → accumulate thinking, watch for (possibly-split) `</think>`; on close drop tags, go `passthrough`, emit trailing same-delta text as text. `passthrough` → everything verbatim text, no more scanning.
Buffer only `len("</think>")` tail — never whole responses (honors translate/CLAUDE.md no-buffering invariant).

Tests (`think_tag_test.go`): clean `<think>x</think>answer`; `<th`+`ink>` split; `</th`+`ink>` split; leading ws/newlines; no tag (passthrough); mid-prose `<think>` stays text; unclosed → all thinking via Flush; text trailing `</think>` same delta.

## Phase 2 — Streaming (`stream.go`)
In `emitDelta`, run `content` through splitter (replacing 831-872). Per segment:
- **segThinking** → reuse reasoning machinery (close open text block, open thinking via `emitContentBlockStartThinking`/`thinkingOpen`, `emitContentBlockDeltaThinking`). Factor the block-management at 813-828 into shared `appendThinking(text)`/`appendText(text)` helpers used by both the `reasoning` branch and the splitter.
- **segText** → existing path (keep pendingText whitespace buffering 840-871 intact).

Add `splitter thinkTagSplitter` field, gated by Phase 4 flag. Call `splitter.Flush()` in `finishStream` before the text/thinking close (1185-1196).

Nudge interaction: rerouting `<think>` means `sawText`/`leadingContent` reflect only the real answer (more correct for `synthesizeTextOnlyTurnNudge`). Verify "MiMo emits only `<think>…</think>`": `sawText=false`+`requestHadTools`+`finish_reason=stop` now fires the nudge — confirm acceptable or guard. Update comment `stream.go:1024-1032`.

Tests (`stream_think_test.go`): split-`<think>` chunk sequence → `content_block_start{thinking}` → `thinking_delta`s → `content_block_stop` → `content_block_start{text}`; mid-prose stays text; thinking-then-tool_call ordering.

## Phase 3 — Non-streaming (`translate.go`)
In `writeAnthropicContentFromOpenAI` (233-254): after existing reasoning emission, run `message.content` through splitter (`Feed`+`Flush` on full string). Emit `thinking` block for extracted segment(s), `text` block for remainder, instead of single text block (247-254). No leading `<think>` → unchanged.

Tests (extend `crossformat_response_test.go`): `<think>…</think>answer` → thinking+text blocks; no-think unchanged.

## Phase 4 — Gating (default-on for MiMo only)
1. Add `WithThinkTagReasoning(bool)` builder on translator (pattern: `WithRequestHadTools` stream.go:505); bypass splitter entirely when off.
2. Add `ThinkTagReasoning bool` field on `catalog.Model` (true for `xiaomi/mimo-v2.5-pro`). Single source of truth — prefer over a magic `Contains("mimo")` substring.
3. Plumb in `proxy/service.go`: streaming call site 1448 (Gemini-chain 1492 stays off); non-streaming via `Finalize`→`openAIToAnthropicResponse` (stream.go:694).

## Phase 5 — Docs
Update `internal/translate/CLAUDE.md` (note `<think>` content-channel extraction) and comment `stream.go:1024-1032`.

---

## Confirm before/during
- Gating: catalog field (recommended) vs model-prefix substring.
- Only-`<think>` turns firing nudge (Phase 2) — desired or guard?
- Unclosed `<think>` on `finish_reason=length` → emitted as thinking via Flush (acceptable).

## Follow-ups (not here)
- OpenAI-inbound passthrough to MiMo: same-format, `<think>` passes through; could add same-format `content`→`reasoning_content` transform.
- Other vLLM/DeepInfra models with same leak: flip their catalog flag, no code change.

Start with Phase 1 (self-contained, de-risks the rest).
