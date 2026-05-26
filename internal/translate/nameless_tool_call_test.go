package translate_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// A nameless tool_call is the cross-model loop trap: GLM, Qwen, Kimi, and
// gpt-oss on vLLM/SGLang/DeepInfra all intermittently emit a tool_call with a
// blank function name (often closing the turn with finish_reason="stop").
// Forwarding it as a tool_use block makes Claude Code invoke tool "" ->
// "No such tool available" -> retry -> infinite loop until the proxy's
// no-progress breaker trips. Both the streaming and non-streaming translators
// must drop the nameless call and leave the turn's real stop_reason intact.

// driveAnthropicSSE feeds OpenAI chat.completion.chunk events through the
// translator and returns the translated Anthropic SSE body.
func driveAnthropicSSE(t *testing.T, model string, events []string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	translator := translate.NewAnthropicSSETranslator(rec, model, nil)
	translator.Header().Set("Content-Type", "text/event-stream")
	translator.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, err := translator.Write([]byte(e))
		require.NoError(t, err)
	}
	return rec.Body.String()
}

func TestAnthropicSSETranslator_DropsNamelessToolCall(t *testing.T) {
	// Every OpenAI-compat upstream we route to can produce this shape, so the
	// guard must be model-agnostic, not keyed on a model name.
	models := []string{"z-ai/glm-5.1", "qwen/qwen3-coder", "moonshotai/kimi-k2", "openai/gpt-oss-120b"}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			body := driveAnthropicSSE(t, model, []string{
				`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"The proof is complete."},"finish_reason":null}]}` + "\n\n",
				// Nameless tool_call: function.name absent. The model is actually done.
				`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
				`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n",
				"data: [DONE]\n\n",
			})

			assert.NotContains(t, body, `"type":"tool_use"`, "nameless tool_call must not become a tool_use block")
			assert.Contains(t, body, "The proof is complete.", "streamed text must survive")
			// No tool_use block => no promotion => the real stop_reason wins.
			assert.Contains(t, body, `"stop_reason":"end_turn"`)
			assert.NotContains(t, body, `"stop_reason":"tool_use"`)
		})
	}
}

func TestAnthropicSSETranslator_DropsNamelessToolCallWithArgFragments(t *testing.T) {
	// Arguments stream in later chunks that reuse the same index but carry no
	// name. A naive guard that only checks the first chunk would still leak a
	// content_block_delta into a stale/zero block index.
	body := driveAnthropicSSE(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.NotContains(t, body, `"type":"tool_use"`)
	assert.NotContains(t, body, "content_block_delta", "no args delta may stream for a dropped tool block")
	assert.Contains(t, body, `"stop_reason":"end_turn"`)
}

func TestAnthropicSSETranslator_KeepsNamedToolDropsNameless(t *testing.T) {
	// A legitimate named tool_call alongside a nameless one: the named call
	// must survive (and promote stop_reason to tool_use), the nameless dropped.
	body := driveAnthropicSSE(t, "z-ai/glm-5.1", []string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_good","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_bad","type":"function","function":{"name":"","arguments":""}}]},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})

	assert.Equal(t, 1, strings.Count(body, `"type":"tool_use"`), "exactly the named tool_use block survives")
	assert.Contains(t, body, `"name":"Read"`)
	assert.NotContains(t, body, `"name":""`)
	// Upstream finish_reason="stop" but a real tool_use block => promotion.
	assert.Contains(t, body, `"stop_reason":"tool_use"`)
}

func TestOpenAIToAnthropicResponse_DropsNamelessToolCall(t *testing.T) {
	// Non-streaming twin of the streaming loop trap.
	resp := []byte(`{
		"id": "chatcmpl-x",
		"object": "chat.completion",
		"model": "z-ai/glm-5.1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "Done.", "tool_calls": [
			{"id": "call_bad", "type": "function", "function": {"name": "", "arguments": ""}}
		]}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "z-ai/glm-5.1")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	// No tool_use block => promotion must not fire => end_turn, not tool_use.
	assert.Equal(t, "end_turn", doc["stop_reason"])
	for _, raw := range content(t, doc) {
		blk, _ := raw.(map[string]any)
		assert.NotEqual(t, "tool_use", blk["type"], "nameless tool_call must be dropped")
	}
}

func TestOpenAIToAnthropicResponse_KeepsNamedToolDropsNameless(t *testing.T) {
	resp := []byte(`{
		"id": "chatcmpl-y",
		"object": "chat.completion",
		"model": "z-ai/glm-5.1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_good", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"a.go\"}"}},
			{"id": "call_bad", "type": "function", "function": {"name": "", "arguments": ""}}
		]}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 3, "total_tokens": 13}
	}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "z-ai/glm-5.1")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "tool_use", doc["stop_reason"], "a surviving named tool_call still promotes")
	blocks := content(t, doc)
	require.Len(t, blocks, 1, "only the named tool_use survives")
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", blk["type"])
	assert.Equal(t, "Read", blk["name"])
}
