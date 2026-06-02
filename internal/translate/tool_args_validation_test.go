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
// truncation. The translator can't repair the payload at content_block_stop
// time without breaking the already-streamed input_json_delta fragments, but
// it MUST surface a structured signal so the proxy can count malformed-tool
// turns from logs. validateBufferedToolArgs is that signal; these tests
// pin its semantics.

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

	// The tool_use block still emits so the response stays protocol-valid;
	// drop is deliberately a follow-up gated on the frequency this surfaces.
	assert.Contains(t, body, `"type":"tool_use"`)
	assert.Contains(t, body, `"name":"Edit"`)
	assert.Equal(t, 1, summary.InvalidToolArgsBlocks,
		"truncated JSON args must be flagged in Summary so the proxy can log the malformed turn")
}

func TestAnthropicSSETranslator_AcceptsValidToolArgs(t *testing.T) {
	_, summary := driveAnthropicSSEWithSummary(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"Edit","arguments":"{\"path\":\"a.go\","}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"old_string\":\"x\",\"new_string\":\"y\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 0, summary.InvalidToolArgsBlocks,
		"well-formed JSON args concatenated across fragments must not be flagged")
	assert.Equal(t, 1, summary.ToolUseBlocks)
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
