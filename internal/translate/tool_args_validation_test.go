package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// OpenAI-compat upstreams (vLLM/SGLang) sometimes emit malformed JSON in
// tool_call.function.arguments. The translator buffers args per block and at
// content_block_stop emits either the valid payload, a minimal deterministic
// repair, or `{}` as last resort — turning a stream-parser-fatal turn (CC's
// strict parser rejects malformed input_json_delta) into a dispatchable call
// that errors cleanly and triggers a normal CC retry.

// driveAnthropicSSEWithSummary feeds events through a translator, returning
// the translated body and Summary for assertions.
func driveAnthropicSSEWithSummary(
	t *testing.T,
	model string,
	events []string,
) (string, translate.ResponseSummary) {
	t.Helper()
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, model, nil)
	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())
	return rec.Body.String(), translator.Summary()
}

func TestAnthropicSSETranslator_RepairsTruncatedToolArgs(t *testing.T) {
	// Truncated JSON (brace/key/colon/string then EOF) is the common GLM/Kimi
	// max_tokens-cutoff shape; repair closes it, preserving intact args instead of `{}`.
	body, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"a.go\",\"old_string\":\"hel"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	// Tool_use block still emits so stop_reason="tool_use" is honored.
	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"name":"Edit"`)
	assert.Equal(t, 0, summary.InvalidToolArgsBlocks,
		"a repaired payload is dispatchable — the unrecoverable-args latch must not fire")
	require.Len(t, summary.ToolCallIssues, 1,
		"the repair must still be reported so the proxy logs the malformed turn")
	assert.Equal(t, "invalid_json", string(summary.ToolCallIssues[0].Bucket))
	assert.True(t, summary.ToolCallIssues[0].Repaired)

	// The raw malformed bytes must not reach the client — only the closed,
	// valid repair does (one consolidated input_json_delta).
	assert.Contains(t, body, `"partial_json":"{\"path\":\"a.go\",\"old_string\":\"hel\"}"`,
		"repair closes the truncated string/brace and keeps the intact args")
}

func TestAnthropicSSETranslator_SubstitutesEmptyArgsWhenUnrepairable(t *testing.T) {
	// Mismatched closer can't be minimally repaired, so the translator falls
	// back to `{}`; CC's parser accepts it, the tool errors on missing params,
	// and CC retries normally instead of dying mid-stream.
	body, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"a.go\"]"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Equal(t, 1, summary.InvalidToolArgsBlocks,
		"unrepairable args must be flagged in Summary so the proxy can log the malformed turn")
	assert.NotContains(t, body, `\"path\":\"a.go\"]`,
		"the malformed args fragment must not reach the client")
	assert.Contains(t, body, `"partial_json":"{}"`,
		"unrepairable args must be substituted with empty `{}` payload so CC's parser succeeds")
}

func TestAnthropicSSETranslator_AcceptsValidToolArgs(t *testing.T) {
	body, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"a.go\","}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"old_string\":\"x\",\"new_string\":\"y\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 0, summary.InvalidToolArgsBlocks,
		"well-formed JSON args concatenated across fragments must not be flagged")
	assert.Equal(t, 1, summary.ToolUseBlocks)
	// Valid args flow through verbatim — concatenated into a single
	// input_json_delta at content_block_stop time.
	assert.Contains(t, body, `"partial_json":"{\"path\":\"a.go\",\"old_string\":\"x\",\"new_string\":\"y\"}"`,
		"valid buffered args must reach the client unchanged, as one delta")
}

func TestAnthropicSSETranslator_FlagsEachInvalidBlockIndependently(t *testing.T) {
	// Two tool_calls, one valid and one unrepairable. Each block is
	// buffered/validated under its own content-block index, so the count tracks blocks not turns.
	_, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"b.go\"]"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 2, summary.ToolUseBlocks)
	assert.Equal(t, 1, summary.InvalidToolArgsBlocks,
		"only the malformed block must be flagged; the valid sibling is unaffected")
	require.Len(t, summary.ToolCallIssues, 1)
	assert.Equal(t, "Edit", summary.ToolCallIssues[0].ToolName)
}

// driveAnthropicSSEWithTools is driveAnthropicSSEWithSummary plus a "had
// tools" flag, for exercising the nudge-synthesis path.
func driveAnthropicSSEWithTools(
	t *testing.T,
	model string,
	hadTools bool,
	events []string,
) (string, translate.ResponseSummary) {
	t.Helper()
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, model, nil).
		WithRequestHadTools(hadTools)
	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	require.NoError(t, translator.Finalize())
	return rec.Body.String(), translator.Summary()
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SynthesizesBash(t *testing.T) {
	// Upstream (e.g. Mimo-v2.5) leaks a tool call as XML text instead of a
	// structured tool_calls entry. finishStream must synthesize a Bash
	// tool_use so CC's loop doesn't die on "tool call could not be parsed"
	// (Gemini-3.x excluded, see _SuppressedOnGemini3x below).
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<tool_call>{\"name\":\"Read\",\"path\":\"a.go\"}</tool_call>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.True(t, summary.TextOnlyTurnNudged,
		"upstream emitted no tool_use on a request with tools — nudge must fire")
	assert.Equal(t, 1, summary.ToolUseBlocks,
		"after the nudge the response carries exactly one synthetic tool_use block")
	assert.Equal(t, "tool_use", summary.StopReason,
		"stop_reason promotes to tool_use so Claude Code dispatches the synthetic call")
	assert.Contains(t, body, `"name":"Bash"`, "synthetic call routes through Bash")
	assert.Contains(t, body, "previous turn produced no tool_use",
		"nudge text instructs the model to switch to real tools")
	assert.Contains(t, body, `"id":"toolu_router_nudge_`,
		"synthetic id is prefixed so log auditors can match it in stream transcripts")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedOnVisibleThinkingThenAnswer(t *testing.T) {
	// Regression (session 1f2ce8be): Mimo-v2.5 streams <think>…</think> then a
	// real prose answer with finish_reason=stop — a valid, complete turn. The
	// old detector treated leading <think> as a failure mode, stapled on a
	// synthetic Bash call, and looped the session. Nudge must NOT fire: <think>
	// is reasoning text, not a leaked tool call.
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<think>Let me look at the file…</think>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Good catch — the sqlc.yml override does map uuid to google/uuid."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"a <think> lead followed by a real answer is a valid turn — nudge must not fire")
	assert.Equal(t, 0, summary.ToolUseBlocks, "no synthetic tool_use on a legitimate answer")
	assert.Equal(t, "end_turn", summary.StopReason, "turn ends naturally, not promoted to tool_use")
	assert.NotContains(t, body, "toolu_router_nudge_")
	assert.Contains(t, body, "Good catch", "the model's real answer survives")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SuppressedOnGemini3x(t *testing.T) {
	// The synthetic Bash block has no thoughtSignature. On Gemini-3.x the next
	// turn drops the entire tool_use/tool_result history (dropToolBlocks in
	// emit_gemini.go), wiping context and looping to the turn ceiling — so the
	// nudge must be suppressed on Gemini-3.x even though the upstream leaked a tool call as text.
	body, summary := driveAnthropicSSEWithTools(t, "gemini-3.1-pro-preview", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<tool_call>{\"name\":\"Read\",\"path\":\"a.go\"}</tool_call>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"nudge must be suppressed on Gemini-3.x — a sig-less tool_use poisons the next turn's history")
	assert.Equal(t, 0, summary.ToolUseBlocks,
		"no synthetic tool_use is emitted on the Gemini-3.x path")
	assert.NotContains(t, body, "toolu_router_nudge_",
		"no synthetic nudge block reaches a Gemini-3.x client")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_NoToolsInRequest(t *testing.T) {
	// No tools in the request means nothing else to do — nudge must not
	// fire (would inject a Bash call the client never declared).
	body, summary := driveAnthropicSSEWithTools(t, "claude-opus-4-8", false, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Here is the explanation you asked for."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"text-only response is correct when no tools were available")
	assert.Equal(t, 0, summary.ToolUseBlocks)
	assert.NotContains(t, body, "toolu_router_nudge_",
		"no synthetic block when tools weren't available")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedWhenToolUseAlreadyEmitted(t *testing.T) {
	// Normal happy path: model emitted a real tool_use. Nudge must skip.
	_, summary := driveAnthropicSSEWithTools(t, "claude-opus-4-8", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"a real tool_use was emitted; nudge must not fire")
	assert.Equal(t, 1, summary.ToolUseBlocks, "exactly one real tool_use, no synthetic addition")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedOnCleanProseFinalAnswer(t *testing.T) {
	// False positive this guards against: clean prose with finish_reason=stop
	// and no tool-call markup is a legitimate completion — stapling on a Bash
	// call would revive an already-finished turn. Nudge must NOT fire.
	body, summary := driveAnthropicSSEWithTools(t, "deepseek-v3.2", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"I've finished the refactor and all tests pass."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" Let me know if you need anything else."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"clean prose finish_reason=stop is a real final answer — nudge must not fire")
	assert.Equal(t, 0, summary.ToolUseBlocks, "no synthetic tool_use on a legitimate completion")
	assert.Equal(t, "end_turn", summary.StopReason, "turn ends naturally, not promoted to tool_use")
	assert.NotContains(t, body, "toolu_router_nudge_")
	assert.Contains(t, body, "I've finished the refactor", "the model's real answer survives")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_FiresWhenLeadingWithToolishMarkup(t *testing.T) {
	// Turn opens with a tool call leaked as plain text (finish_reason=stop
	// notwithstanding); leading markup is the discriminator that keeps the nudge firing.
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<tool_call>{\"name\":\"Edit\"}</tool_call>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.True(t, summary.TextOnlyTurnNudged,
		"a turn leading with tool-call markup is the failure mode — nudge must fire despite finish_reason=stop")
	assert.Equal(t, 1, summary.ToolUseBlocks)
	assert.Contains(t, body, `"id":"toolu_router_nudge_`)
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedWhenLeadingWithRedactedThinking(t *testing.T) {
	// <redacted_thinking>, like <think>, is reasoning text, not a leaked tool
	// call. Nudging it manufactured the production loop (session 1f2ce8be).
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<redacted_thinking>opaque</redacted_thinking>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" Here is the answer."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":15}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"redacted thinking is reasoning text, not leaked tool-call markup — nudge must not fire")
	assert.Equal(t, 0, summary.ToolUseBlocks)
	assert.NotContains(t, body, `"id":"toolu_router_nudge_`)
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedWhenProseMentionsMarkup(t *testing.T) {
	// Substring-match trap: prose that *discusses* tag syntax mid-sentence
	// isn't a leak since the markup isn't at the start — must not be nudged.
	body, summary := driveAnthropicSSEWithTools(t, "deepseek-v3.2", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"The detector trips when content begins with a <think> tag, "},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"so prose mentioning <tool_call> or <function> mid-sentence is fine."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":20}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"markup mentioned mid-prose is a real answer, not a leak — nudge must not fire")
	assert.Equal(t, 0, summary.ToolUseBlocks)
	assert.NotContains(t, body, "toolu_router_nudge_")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_FiresOnToolCallsFinish(t *testing.T) {
	// finish_reason=tool_calls means the upstream signaled a tool call that
	// never materialized — nudge is correct despite clean visible prose.
	body, summary := driveAnthropicSSEWithTools(t, "z-ai/glm-5.1", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"I'll create the PR now."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":6}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.True(t, summary.TextOnlyTurnNudged,
		"finish_reason=tool_calls with no structured call — the model wanted a tool; nudge must fire")
	assert.Equal(t, 1, summary.ToolUseBlocks)
	assert.Contains(t, body, `"id":"toolu_router_nudge_`)
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SkippedOnLengthTruncation(t *testing.T) {
	// finish_reason=length means truncation mid-output; a Bash echo can't
	// help — nudge must not fire.
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"Here is the first part of the plan and then"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":40,"completion_tokens":4096}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"truncated (finish_reason=length) turns must not be nudged")
	assert.Equal(t, 0, summary.ToolUseBlocks)
	assert.NotContains(t, body, "toolu_router_nudge_")
}

func TestAnthropicSSETranslator_TextOnlyTurnNudge_FiresOnEmptyTurn(t *testing.T) {
	// A turn with no text and no tool_use is an empty dead-end, not a final
	// answer. Nudge still fires since the clean-prose guard only suppresses turns with actual prose.
	_, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":0}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.True(t, summary.TextOnlyTurnNudged,
		"an empty turn is a dead-end, not a final answer — nudge must fire")
	assert.Equal(t, 1, summary.ToolUseBlocks)
}

func TestAnthropicSSETranslator_EmptyArgsNotFlagged(t *testing.T) {
	// Some tools take no input, so an empty/missing arguments buffer is the
	// trivial case, not malformed.
	_, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Ping"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 0, summary.InvalidToolArgsBlocks,
		"a tool with no streamed arguments is not the malformed-args case")
}
