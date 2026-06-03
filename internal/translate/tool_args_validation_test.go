package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// OpenAI-compat upstreams on vLLM/SGLang sometimes emit malformed JSON in
// tool_call.function.arguments — partial keys, unbalanced braces, mid-string
// truncation. The translator buffers args per tool block, validates at
// content_block_stop, and emits exactly one input_json_delta carrying either
// the valid buffered payload OR `{}` when validation failed. The `{}`
// substitute converts a stream-parser-fatal turn (Claude Code's strict
// Anthropic parser refuses malformed input_json_delta) into a tool-call the
// client can dispatch — which then errors on missing required params and
// triggers a normal CC retry that re-routes through the scorer.

// driveAnthropicSSEWithSummary feeds events through a translator and returns
// the final response summary alongside the translated body, so tests can
// assert on InvalidToolArgsBlocks.
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

func TestAnthropicSSETranslator_FlagsInvalidToolArgs(t *testing.T) {
	// Truncated JSON: opening brace, key, colon, opening string — then EOF.
	// This is the most common malformed shape seen from GLM/Kimi on vLLM
	// when the model hits the max_tokens cap mid-tool-call.
	body, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"a.go\",\"old_string\":\"hel"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	// Tool_use block still emits so stop_reason="tool_use" is honored.
	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"name":"Edit"`)
	assert.Equal(t, 1, summary.InvalidToolArgsBlocks,
		"truncated JSON args must be flagged in Summary so the proxy can log the malformed turn")

	// The translator MUST NOT forward the malformed bytes to the client.
	// Instead it emits a single input_json_delta carrying `{}`, which CC's
	// parser accepts. The downstream tool call then errors on missing
	// required params, which CC handles with its standard tool-result retry
	// instead of dying mid-stream on "tool call could not be parsed."
	assert.NotContains(t, body, `\"old_string\":\"hel`,
		"the malformed args fragment must not reach the client")
	assert.Contains(t, body, `"partial_json":"{}"`,
		"invalid args must be substituted with empty `{}` payload so CC's parser succeeds")
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
	// Two tool_calls: one with valid args, one truncated. Each block is
	// buffered + validated under its own Anthropic content-block index, so
	// the count tracks blocks not turns.
	_, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"b.go"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 2, summary.ToolUseBlocks)
	assert.Equal(t, 1, summary.InvalidToolArgsBlocks,
		"only the truncated block must be flagged; the valid sibling is unaffected")
}

// driveAnthropicSSEWithTools is driveAnthropicSSEWithSummary plus an
// explicit "request had tools" flag, enabling the text-only-turn nudge
// synthesis path.
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
	// OpenAI-compat failure mode (e.g. Mimo-v2.5): upstream emits prose +
	// <think> XML as plain text deltas, no tool_calls. Request HAD tools
	// available. finishStream must synthesize a Bash tool_use so Claude
	// Code's loop doesn't die on "tool call could not be parsed". (Gemini-3.x
	// is deliberately excluded — see the _SuppressedOnGemini3x test below.)
	body, summary := driveAnthropicSSEWithTools(t, "mimo-v2.5-pro", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<think>Let me look at the file…</think>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" I will read the relevant code first."},"finish_reason":null}]}` + "\n\n",
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

func TestAnthropicSSETranslator_TextOnlyTurnNudge_SuppressedOnGemini3x(t *testing.T) {
	// Regression: the synthetic Bash block has no thoughtSignature. On
	// Gemini-3.x the next turn drops the ENTIRE tool_use/tool_result history
	// (anyToolUseMissingSig → dropToolBlocks in emit_gemini.go), wiping the
	// agent's working context and looping it to the turn ceiling. So even
	// though the request had tools and the upstream emitted only text, the
	// nudge MUST be suppressed when the routed model is Gemini-3.x.
	body, summary := driveAnthropicSSEWithTools(t, "gemini-3.1-pro-preview", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"<think>Let me look at the file…</think>"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":" I will read the relevant code first."},"finish_reason":null}]}` + "\n\n",
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
	// When the inbound request had no tools the model legitimately had
	// nothing else to do — nudge must NOT fire (would inject a Bash call
	// the client never declared).
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

func TestAnthropicSSETranslator_EmptyArgsNotFlagged(t *testing.T) {
	// Some tools take no input. The upstream emits no arguments fragment at
	// all (or an empty one). An empty buffer is the trivial case — not
	// malformed, not flagged.
	_, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Ping"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 0, summary.InvalidToolArgsBlocks,
		"a tool with no streamed arguments is not the malformed-args case")
}
