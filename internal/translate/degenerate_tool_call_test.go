package translate_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// The degenerate tool-call turn: an OpenAI-compat upstream (GLM-5.1 on
// DeepInfra is the repeat offender) closes with finish_reason="tool_calls" but
// no usable tool_use block ever materializes — every call was nameless and
// dropped, or the call leaked into plain text the parser never structured.
// finish_reason="tool_calls" maps to stop_reason="tool_use", so without a
// demote the client receives stop_reason="tool_use" alongside zero tool_use
// blocks. Agent clients (Claude Code, pi) then wait for a tool call that never
// arrives and the turn dead-ends, which surfaces to the user as the agent
// "stopping instead of going". The translators must demote to end_turn.

// driveAnthropicSSEWithTools (declared in tool_args_validation_test.go) feeds
// OpenAI chunks through the translator and returns the body plus the summary.

func TestAnthropicSSETranslator_DemotesToolCallsFinishWithNamelessCall(t *testing.T) {
	// finish_reason="tool_calls" + a nameless (dropped) call. requestHadTools
	// is false so the text-only nudge cannot mask the demote: this is the
	// no-tools path the nudge deliberately skips.
	body, summary := driveAnthropicSSEWithTools(t, "z-ai/glm-5.1", false, []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Let me run that for you."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.NotContains(t, body, `"type":"tool_use"`, "nameless tool_call must not become a tool_use block")
	assert.Contains(t, body, `"stop_reason":"end_turn"`, "tool_calls finish with no surviving tool_use block must demote to end_turn")
	assert.NotContains(t, body, `"stop_reason":"tool_use"`)

	assert.Equal(t, "tool_calls", summary.UpstreamFinishReason)
	assert.Equal(t, "end_turn", summary.StopReason)
	assert.Equal(t, 0, summary.ToolUseBlocks)
	assert.True(t, summary.StopReasonDemoted, "demote must be recorded for observability")
	assert.Equal(t, 1, summary.SuppressedToolCalls, "the nameless call is the suppression that drove the demote")
}

func TestAnthropicSSETranslator_SuppressedToolCallFinishDoesNotNudge(t *testing.T) {
	// The prod loop (session 081e0a3d, model z-ai/glm-5.1 on DeepInfra): the
	// upstream closes finish_reason="tool_calls" with a nameless call that gets
	// dropped, AND the request HAD tools available. Without the suppressed-call
	// guard, synthesizeTextOnlyTurnNudge fires (its switch has no "tool_calls"
	// case), stapling a synthetic Bash call onto every such turn. Because the
	// degenerate shape recurs each turn, the nudge loops to the turn ceiling.
	// The model already emitted a (malformed) tool call; the drop handled it, so
	// the nudge must stay silent.
	body, summary := driveAnthropicSSEWithTools(t, "z-ai/glm-5.1", true, []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.False(t, summary.TextOnlyTurnNudged,
		"a suppressed (nameless) tool_call must not trigger the text-only nudge")
	assert.Equal(t, 0, summary.ToolUseBlocks, "no synthetic tool_use block is emitted")
	assert.NotContains(t, body, "toolu_router_nudge_", "no synthetic nudge block reaches the client")
	assert.Equal(t, 1, summary.SuppressedToolCalls)
	assert.Equal(t, "end_turn", summary.StopReason, "tool_calls finish with no surviving block still demotes")
}

func TestAnthropicSSETranslator_DemotesToolCallsFinishWithNoStructuredCall(t *testing.T) {
	// finish_reason="tool_calls" but the upstream emitted only text — the call
	// leaked into prose the parser never structured (SuppressedToolCalls == 0).
	body, summary := driveAnthropicSSEWithTools(t, "z-ai/glm-5.1", false, []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"I'll create the PR now."},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":6}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Contains(t, body, `"stop_reason":"end_turn"`)
	assert.NotContains(t, body, `"stop_reason":"tool_use"`)
	assert.Contains(t, body, "I'll create the PR now.", "streamed text must survive")

	assert.Equal(t, "end_turn", summary.StopReason)
	assert.True(t, summary.StopReasonDemoted)
	assert.Equal(t, 0, summary.SuppressedToolCalls, "no structured call means nothing was suppressed — the leaked-into-text case")
}

func TestAnthropicSSETranslator_NamedToolCallWithToolCallsFinishStillToolUse(t *testing.T) {
	// Regression guard: a legitimate named tool_call closing with
	// finish_reason="tool_calls" must keep stop_reason="tool_use" and never
	// demote.
	body, summary := driveAnthropicSSEWithTools(t, "z-ai/glm-5.1", true, []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_ok","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"git status\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":7}}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"stop_reason":"tool_use"`)

	assert.Equal(t, "tool_use", summary.StopReason)
	assert.Equal(t, 1, summary.ToolUseBlocks)
	assert.False(t, summary.StopReasonDemoted, "a real tool_use block must not be demoted")
}

func TestOpenAIToAnthropicResponse_DemotesToolCallsFinishWithEmptyCalls(t *testing.T) {
	// Non-streaming twin: finish_reason="tool_calls" with an empty tool_calls
	// array. Demote to end_turn rather than ship tool_use with zero blocks.
	resp := []byte(`{
		"id": "chatcmpl-z",
		"object": "chat.completion",
		"model": "z-ai/glm-5.1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "On it.", "tool_calls": []}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "z-ai/glm-5.1")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "end_turn", doc["stop_reason"])
	for _, raw := range content(t, doc) {
		blk, _ := raw.(map[string]any)
		assert.NotEqual(t, "tool_use", blk["type"])
	}
}

func TestOpenAIToAnthropicResponse_DemotesToolCallsFinishWithNamelessCall(t *testing.T) {
	// finish_reason="tool_calls" with only a nameless (dropped) call.
	resp := []byte(`{
		"id": "chatcmpl-z2",
		"object": "chat.completion",
		"model": "z-ai/glm-5.1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Working on it.", "tool_calls": [
			{"id": "call_bad", "type": "function", "function": {"name": "", "arguments": ""}}
		]}, "finish_reason": "tool_calls"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "z-ai/glm-5.1")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "end_turn", doc["stop_reason"])
	for _, raw := range content(t, doc) {
		blk, _ := raw.(map[string]any)
		assert.NotEqual(t, "tool_use", blk["type"])
	}
}
