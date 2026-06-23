package translate_test

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var anthropicTextResponse = []byte(`{
	"id": "msg_01XYZ",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-20250514",
	"content": [{"type": "text", "text": "Hello, world!"}],
	"stop_reason": "end_turn",
	"stop_sequence": null,
	"usage": {"input_tokens": 25, "output_tokens": 10}
}`)

var anthropicToolResponse = []byte(`{
	"id": "msg_02ABC",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-20250514",
	"content": [
		{"type": "tool_use", "id": "toolu_read1", "name": "Read", "input": {"path": "main.go"}}
	],
	"stop_reason": "tool_use",
	"stop_sequence": null,
	"usage": {"input_tokens": 40, "output_tokens": 20}
}`)

var anthropicMultiToolResponse = []byte(`{
	"id": "msg_03DEF",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-20250514",
	"content": [
		{"type": "tool_use", "id": "toolu_r1", "name": "Read", "input": {"path": "a.go"}},
		{"type": "tool_use", "id": "toolu_r2", "name": "Read", "input": {"path": "b.go"}}
	],
	"stop_reason": "tool_use",
	"stop_sequence": null,
	"usage": {"input_tokens": 50, "output_tokens": 30}
}`)

var anthropicMixedResponse = []byte(`{
	"id": "msg_04GHI",
	"type": "message",
	"role": "assistant",
	"model": "claude-sonnet-4-20250514",
	"content": [
		{"type": "text", "text": "Let me read that."},
		{"type": "tool_use", "id": "toolu_m1", "name": "Read", "input": {"path": "main.go"}}
	],
	"stop_reason": "tool_use",
	"stop_sequence": null,
	"usage": {"input_tokens": 60, "output_tokens": 25}
}`)

var openAITextResponse = []byte(`{
	"id": "chatcmpl-abc123",
	"object": "chat.completion",
	"created": 1234567890,
	"model": "gpt-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello, world!"}, "finish_reason": "stop"}],
	"usage": {"prompt_tokens": 25, "completion_tokens": 10, "total_tokens": 35}
}`)

var openAIToolResponse = []byte(`{
	"id": "chatcmpl-def456",
	"object": "chat.completion",
	"created": 1234567890,
	"model": "gpt-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": null, "tool_calls": [
		{"id": "call_r1", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"main.go\"}"}}
	]}, "finish_reason": "tool_calls"}],
	"usage": {"prompt_tokens": 40, "completion_tokens": 20, "total_tokens": 60}
}`)

var openAIMultiToolResponse = []byte(`{
	"id": "chatcmpl-ghi789",
	"object": "chat.completion",
	"created": 1234567890,
	"model": "gpt-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": null, "tool_calls": [
		{"id": "call_a1", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"a.go\"}"}},
		{"id": "call_b1", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"b.go\"}"}}
	]}, "finish_reason": "tool_calls"}],
	"usage": {"prompt_tokens": 50, "completion_tokens": 30, "total_tokens": 80}
}`)

var openAIMixedResponse = []byte(`{
	"id": "chatcmpl-jkl012",
	"object": "chat.completion",
	"created": 1234567890,
	"model": "gpt-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "Let me read that.", "tool_calls": [
		{"id": "call_m1", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"main.go\"}"}}
	]}, "finish_reason": "tool_calls"}],
	"usage": {"prompt_tokens": 60, "completion_tokens": 25, "total_tokens": 85}
}`)

var openAICacheUsageResponse = []byte(`{
	"id": "chatcmpl-mno345",
	"object": "chat.completion",
	"created": 1234567890,
	"model": "gpt-4",
	"choices": [{"index": 0, "message": {"role": "assistant", "content": "Cached."}, "finish_reason": "stop"}],
	"usage": {
		"prompt_tokens": 100,
		"completion_tokens": 10,
		"total_tokens": 110,
		"prompt_tokens_details": {"cached_tokens": 80}
	}
}`)

var geminiTextResponse = []byte(`{
	"candidates": [{"content": {"parts": [{"text": "Hello, world!"}], "role": "model"}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 25, "candidatesTokenCount": 10, "totalTokenCount": 35}
}`)

var geminiToolResponse = []byte(`{
	"candidates": [{"content": {"parts": [
		{"functionCall": {"name": "Read", "args": {"path": "main.go"}}}
	], "role": "model"}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 40, "candidatesTokenCount": 20, "totalTokenCount": 60}
}`)

var geminiMultiToolResponse = []byte(`{
	"candidates": [{"content": {"parts": [
		{"functionCall": {"name": "Read", "args": {"path": "a.go"}}},
		{"functionCall": {"name": "Read", "args": {"path": "b.go"}}}
	], "role": "model"}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 50, "candidatesTokenCount": 30, "totalTokenCount": 80}
}`)

var geminiMixedResponse = []byte(`{
	"candidates": [{"content": {"parts": [
		{"text": "Let me read that."},
		{"functionCall": {"name": "Read", "args": {"path": "main.go"}}}
	], "role": "model"}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 60, "candidatesTokenCount": 25, "totalTokenCount": 85}
}`)

var geminiThoughtSigResponse = []byte(`{
	"candidates": [{"content": {"parts": [
		{"functionCall": {"name": "Read", "args": {"path": "main.go"}}, "thoughtSignature": "OPAQUE_SIG_ABC"}
	], "role": "model"}, "finishReason": "STOP"}],
	"usageMetadata": {"promptTokenCount": 30, "candidatesTokenCount": 15, "totalTokenCount": 45}
}`)

func unmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc
}

func choices(t *testing.T, doc map[string]any) []any {
	t.Helper()
	cs, _ := doc["choices"].([]any)
	require.NotEmpty(t, cs)
	return cs
}

func firstChoice(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	c, _ := choices(t, doc)[0].(map[string]any)
	require.NotNil(t, c)
	return c
}

func message(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	m, _ := firstChoice(t, doc)["message"].(map[string]any)
	require.NotNil(t, m)
	return m
}

func toolCalls(t *testing.T, doc map[string]any) []any {
	t.Helper()
	tcs, _ := message(t, doc)["tool_calls"].([]any)
	return tcs
}

func usage(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	u, _ := doc["usage"].(map[string]any)
	require.NotNil(t, u)
	return u
}

func content(t *testing.T, doc map[string]any) []any {
	t.Helper()
	c, _ := doc["content"].([]any)
	return c
}

func TestAnthropicToOpenAIResponse_SimpleText(t *testing.T) {
	out, err := translate.AnthropicToOpenAIResponse(anthropicTextResponse, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "msg_01XYZ", doc["id"])
	assert.Equal(t, "chat.completion", doc["object"])
	assert.Equal(t, "claude-sonnet-4-20250514", doc["model"])

	msg := message(t, doc)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "Hello, world!", msg["content"])

	choice := firstChoice(t, doc)
	assert.Equal(t, "stop", choice["finish_reason"])

	u := usage(t, doc)
	assert.Equal(t, float64(25), u["prompt_tokens"])
	assert.Equal(t, float64(10), u["completion_tokens"])
	assert.Equal(t, float64(35), u["total_tokens"])
}

func TestAnthropicToOpenAIResponse_ToolUse(t *testing.T) {
	out, err := translate.AnthropicToOpenAIResponse(anthropicToolResponse, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "tool_calls", firstChoice(t, doc)["finish_reason"])

	msg := message(t, doc)
	assert.Nil(t, msg["content"])

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 1)
	tc, _ := tcs[0].(map[string]any)
	assert.Equal(t, "toolu_read1", tc["id"])
	assert.Equal(t, "function", tc["type"])
	assert.NotContains(t, tc, "index")

	fn, _ := tc["function"].(map[string]any)
	assert.Equal(t, "Read", fn["name"])
	args, _ := fn["arguments"].(string)
	assert.True(t, json.Valid([]byte(args)), "arguments must be valid JSON string")
	assert.Contains(t, args, "main.go")
}

func TestAnthropicToOpenAIResponse_MultipleToolCalls(t *testing.T) {
	out, err := translate.AnthropicToOpenAIResponse(anthropicMultiToolResponse, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 2)

	ids := make(map[string]bool)
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		id, _ := tc["id"].(string)
		require.NotEmpty(t, id)
		ids[id] = true
		fn, _ := tc["function"].(map[string]any)
		args, _ := fn["arguments"].(string)
		assert.True(t, json.Valid([]byte(args)))
	}
	assert.Len(t, ids, 2, "tool call IDs must be unique")
}

func TestAnthropicToOpenAIResponse_MixedTextAndToolCalls(t *testing.T) {
	out, err := translate.AnthropicToOpenAIResponse(anthropicMixedResponse, "claude-sonnet-4-20250514")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	msg := message(t, doc)
	assert.Equal(t, "Let me read that.", msg["content"])

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 1)
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	assert.Equal(t, "Read", fn["name"])
}

func TestAnthropicToOpenAIResponse_StopReasonMapping(t *testing.T) {
	cases := []struct{ anthropic, openai string }{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"tool_use", "tool_calls"},
		{"stop_sequence", "stop"},
	}
	for _, c := range cases {
		body := `{"id":"x","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"` + c.anthropic + `"}`
		out, err := translate.AnthropicToOpenAIResponse([]byte(body), "m")
		require.NoError(t, err)
		doc := unmarshal(t, out)
		assert.Equal(t, c.openai, firstChoice(t, doc)["finish_reason"],
			"anthropic %q → openai %q", c.anthropic, c.openai)
	}
}

func TestAnthropicToOpenAIResponse_UsageTranslation(t *testing.T) {
	body := []byte(`{"id":"x","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":30,"output_tokens":15}}`)
	out, err := translate.AnthropicToOpenAIResponse(body, "m")
	require.NoError(t, err)
	u := usage(t, unmarshal(t, out))
	assert.Equal(t, float64(30), u["prompt_tokens"])
	assert.Equal(t, float64(15), u["completion_tokens"])
	assert.Equal(t, float64(45), u["total_tokens"])
}

func TestAnthropicToOpenAIResponse_MissingUsageReturnsZero(t *testing.T) {
	body := []byte(`{"id":"x","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn"}`)
	out, err := translate.AnthropicToOpenAIResponse(body, "m")
	require.NoError(t, err)
	u := usage(t, unmarshal(t, out))
	assert.Equal(t, float64(0), u["prompt_tokens"])
	assert.Equal(t, float64(0), u["completion_tokens"])
	assert.Equal(t, float64(0), u["total_tokens"])
}
func TestOpenAIToAnthropicResponse_SimpleText(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAITextResponse, "gpt-4")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "chatcmpl-abc123", doc["id"])
	assert.Equal(t, "message", doc["type"])
	assert.Equal(t, "assistant", doc["role"])
	assert.Equal(t, "gpt-4", doc["model"])
	assert.Equal(t, "end_turn", doc["stop_reason"])

	blocks := content(t, doc)
	require.Len(t, blocks, 1)
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "text", blk["type"])
	assert.Equal(t, "Hello, world!", blk["text"])
}

func TestOpenAIToAnthropicResponse_ToolUse(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAIToolResponse, "gpt-4")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "tool_use", doc["stop_reason"])

	blocks := content(t, doc)
	require.Len(t, blocks, 1)
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", blk["type"])
	assert.Equal(t, "call_r1", blk["id"])
	assert.Equal(t, "Read", blk["name"])

	input, _ := blk["input"].(map[string]any)
	require.NotNil(t, input, "Anthropic input must be an object, not a string")
	assert.Equal(t, "main.go", input["path"])
}

func TestOpenAIToAnthropicResponse_MultipleToolCalls(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAIMultiToolResponse, "gpt-4")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	blocks := content(t, doc)
	require.Len(t, blocks, 2)

	ids := make(map[string]bool)
	for _, raw := range blocks {
		blk, _ := raw.(map[string]any)
		assert.Equal(t, "tool_use", blk["type"])
		id, _ := blk["id"].(string)
		ids[id] = true
		input, _ := blk["input"].(map[string]any)
		require.NotNil(t, input)
	}
	assert.Len(t, ids, 2, "tool_use IDs must be unique")
}

func TestOpenAIToAnthropicResponse_MixedTextAndToolCalls(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAIMixedResponse, "gpt-4")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	blocks := content(t, doc)
	require.Len(t, blocks, 2)

	textBlk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "text", textBlk["type"])
	assert.Equal(t, "Let me read that.", textBlk["text"])

	toolBlk, _ := blocks[1].(map[string]any)
	assert.Equal(t, "tool_use", toolBlk["type"])
	assert.Equal(t, "call_m1", toolBlk["id"])
}

func TestOpenAIToAnthropicResponse_StopReasonMapping(t *testing.T) {
	// finish_reason="tool_calls" is intentionally absent: with no tool_use
	// block it demotes to end_turn, and with one it promotes to tool_use — a
	// context-dependent mapping covered by the Promotes/Demotes tests below.
	// The cases here use a tool-free message body, so only the content-agnostic
	// mappings belong.
	cases := []struct{ openai, anthropic string }{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
	}
	for _, c := range cases {
		body := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"` + c.openai + `"}]}`
		out, err := translate.OpenAIToAnthropicResponse([]byte(body), "m")
		require.NoError(t, err)
		doc := unmarshal(t, out)
		assert.Equal(t, c.anthropic, doc["stop_reason"],
			"openai %q → anthropic %q", c.openai, c.anthropic)
	}
}

// Anthropic invariant: when the OpenAI message carries tool_calls, the
// translated response must report stop_reason="tool_use" regardless of the
// upstream finish_reason. GLM-5.1 on DeepInfra and other vLLM-backed OpenAI-
// compat serves close tool-emitting turns with finish_reason="stop"; without
// promotion the client sees tool_use blocks + stop_reason="end_turn" and
// Claude Code loops on the partial tool call.
func TestOpenAIToAnthropicResponse_PromotesStopReasonWhenToolCallsPresent(t *testing.T) {
	body := []byte(`{
		"id": "chatcmpl-glm",
		"object": "chat.completion",
		"model": "z-ai/glm-5.1",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_glm", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"a.go\"}"}}
		]}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 40, "completion_tokens": 20, "total_tokens": 60}
	}`)
	out, err := translate.OpenAIToAnthropicResponse(body, "z-ai/glm-5.1")
	require.NoError(t, err)
	doc := unmarshal(t, out)
	assert.Equal(t, "tool_use", doc["stop_reason"],
		"tool_calls present → stop_reason must be tool_use even when upstream finish_reason=stop")
}

func TestOpenAIToAnthropicResponse_UsageTranslation(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAITextResponse, "gpt-4")
	require.NoError(t, err)
	u, _ := unmarshal(t, out)["usage"].(map[string]any)
	require.NotNil(t, u)
	assert.Equal(t, float64(25), u["input_tokens"])
	assert.Equal(t, float64(10), u["output_tokens"])
}

func TestOpenAIToAnthropicResponse_CachedUsageTranslation(t *testing.T) {
	out, err := translate.OpenAIToAnthropicResponse(openAICacheUsageResponse, "gpt-4")
	require.NoError(t, err)
	u, _ := unmarshal(t, out)["usage"].(map[string]any)
	require.NotNil(t, u)
	// 100 prompt_tokens - 80 cached = 20 fresh input_tokens
	assert.Equal(t, float64(20), u["input_tokens"])
	assert.Equal(t, float64(10), u["output_tokens"])
	assert.Equal(t, float64(80), u["cache_read_input_tokens"])
}

func TestOpenAIToAnthropicResponse_FallbackModelFromRequest(t *testing.T) {
	body := []byte(`{"id":"chatcmpl-x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
	out, err := translate.OpenAIToAnthropicResponse(body, "fallback-model")
	require.NoError(t, err)
	doc := unmarshal(t, out)
	assert.Equal(t, "fallback-model", doc["model"])
}
func TestGeminiToOpenAIResponse_SimpleText(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiTextResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "chat.completion", doc["object"])
	assert.Equal(t, "gemini-2.5-flash", doc["model"])
	id, _ := doc["id"].(string)
	assert.True(t, strings.HasPrefix(id, "chatcmpl-"), "id must start with chatcmpl-")

	msg := message(t, doc)
	assert.Equal(t, "assistant", msg["role"])
	assert.Equal(t, "Hello, world!", msg["content"])

	assert.Equal(t, "stop", firstChoice(t, doc)["finish_reason"])

	u := usage(t, doc)
	assert.Equal(t, float64(25), u["prompt_tokens"])
	assert.Equal(t, float64(10), u["completion_tokens"])
	assert.Equal(t, float64(35), u["total_tokens"])
}

func TestGeminiToOpenAIResponse_ToolUse(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiToolResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "tool_calls", firstChoice(t, doc)["finish_reason"])

	msg := message(t, doc)
	assert.Nil(t, msg["content"])

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 1)
	tc, _ := tcs[0].(map[string]any)
	assert.Equal(t, "function", tc["type"])
	id, _ := tc["id"].(string)
	assert.NotEmpty(t, id)

	fn, _ := tc["function"].(map[string]any)
	assert.Equal(t, "Read", fn["name"])
	args, _ := fn["arguments"].(string)
	assert.True(t, json.Valid([]byte(args)), "arguments must be valid JSON string")
	assert.Contains(t, args, "main.go")
}

func TestGeminiToOpenAIResponse_MultipleToolCalls(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiMultiToolResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 2)

	ids := make(map[string]bool)
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		id, _ := tc["id"].(string)
		assert.NotEmpty(t, id)
		ids[id] = true
		fn, _ := tc["function"].(map[string]any)
		args, _ := fn["arguments"].(string)
		assert.True(t, json.Valid([]byte(args)))
	}
	assert.Len(t, ids, 2, "tool call IDs must be unique")
}

func TestGeminiToOpenAIResponse_MixedTextAndToolCalls(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiMixedResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	msg := message(t, doc)
	assert.Equal(t, "Let me read that.", msg["content"])

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 1)
}

func TestGeminiToOpenAIResponse_FinishReasonMappingWithToolCalls(t *testing.T) {
	cases := []struct {
		reason       string
		hasToolCalls bool
		expected     string
	}{
		{"STOP", true, "tool_calls"},
		{"MAX_TOKENS", false, "length"},
		{"SAFETY", false, "content_filter"},
	}
	for _, c := range cases {
		var body []byte
		if c.hasToolCalls {
			body = geminiToolResponse
		} else {
			body = []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"` + c.reason + `"}],"usageMetadata":{}}`)
		}
		out, err := translate.GeminiToOpenAIResponse(body, "m")
		require.NoError(t, err)
		doc := unmarshal(t, out)
		assert.Equal(t, c.expected, firstChoice(t, doc)["finish_reason"],
			"gemini %q hasToolCalls=%v → openai %q", c.reason, c.hasToolCalls, c.expected)
	}
}

func TestGeminiToOpenAIResponse_UsageTranslation(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiTextResponse, "m")
	require.NoError(t, err)
	u := usage(t, unmarshal(t, out))
	assert.Equal(t, float64(25), u["prompt_tokens"])
	assert.Equal(t, float64(10), u["completion_tokens"])
	assert.Equal(t, float64(35), u["total_tokens"])
}

func TestGeminiToOpenAIResponse_MissingUsageReturnsZero(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"STOP"}]}`)
	out, err := translate.GeminiToOpenAIResponse(body, "m")
	require.NoError(t, err)
	u := usage(t, unmarshal(t, out))
	assert.Equal(t, float64(0), u["prompt_tokens"])
	assert.Equal(t, float64(0), u["completion_tokens"])
}

func TestGeminiToOpenAIResponse_ThoughtSignaturePreserved(t *testing.T) {
	out, err := translate.GeminiToOpenAIResponse(geminiThoughtSigResponse, "m")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	tcs := toolCalls(t, doc)
	require.Len(t, tcs, 1)
	tc, _ := tcs[0].(map[string]any)

	// The signature rides solely in the tool-call id — the one carrier every
	// client SDK round-trips. No off-spec thought_signature field is emitted.
	assert.NotContains(t, tc, "thought_signature", "off-spec field must not be emitted")
	fn, _ := tc["function"].(map[string]any)
	assert.NotContains(t, fn, "thought_signature", "off-spec field must not be emitted")

	id, _ := tc["id"].(string)
	encoded := base64.RawURLEncoding.EncodeToString([]byte("OPAQUE_SIG_ABC"))
	assert.Contains(t, id, encoded, "signature must be embedded in tool call ID for round-trip")
}
func TestGeminiToAnthropicResponse_SimpleText(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiTextResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "message", doc["type"])
	assert.Equal(t, "assistant", doc["role"])
	assert.Equal(t, "gemini-2.5-flash", doc["model"])
	assert.Equal(t, "end_turn", doc["stop_reason"])
	id, _ := doc["id"].(string)
	assert.True(t, strings.HasPrefix(id, "msg_"), "id must start with msg_")

	blocks := content(t, doc)
	require.Len(t, blocks, 1)
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "text", blk["type"])
	assert.Equal(t, "Hello, world!", blk["text"])
}

func TestGeminiToAnthropicResponse_ToolUse(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiToolResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	assert.Equal(t, "tool_use", doc["stop_reason"])

	blocks := content(t, doc)
	require.Len(t, blocks, 1)
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", blk["type"])
	assert.Equal(t, "Read", blk["name"])

	input, _ := blk["input"].(map[string]any)
	require.NotNil(t, input, "Anthropic input must be an object")
	assert.Equal(t, "main.go", input["path"])

	id, _ := blk["id"].(string)
	assert.True(t, strings.HasPrefix(id, "toolu_"), "tool_use id must start with toolu_")
}

func TestGeminiToAnthropicResponse_MultipleToolCalls(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiMultiToolResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	blocks := content(t, doc)
	require.Len(t, blocks, 2)

	ids := make(map[string]bool)
	for _, raw := range blocks {
		blk, _ := raw.(map[string]any)
		assert.Equal(t, "tool_use", blk["type"])
		id, _ := blk["id"].(string)
		ids[id] = true
		input, _ := blk["input"].(map[string]any)
		require.NotNil(t, input)
	}
	assert.Len(t, ids, 2, "tool_use IDs must be unique")
}

func TestGeminiToAnthropicResponse_MixedTextAndToolCalls(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiMixedResponse, "gemini-2.5-flash")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	blocks := content(t, doc)
	require.Len(t, blocks, 2)

	textBlk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "text", textBlk["type"])
	assert.Equal(t, "Let me read that.", textBlk["text"])

	toolBlk, _ := blocks[1].(map[string]any)
	assert.Equal(t, "tool_use", toolBlk["type"])
}

func TestGeminiToAnthropicResponse_FinishReasonMapping(t *testing.T) {
	cases := []struct {
		reason     string
		hasToolUse bool
		expected   string
	}{
		{"STOP", false, "end_turn"},
		{"STOP", true, "tool_use"},
		{"MAX_TOKENS", false, "max_tokens"},
		{"SAFETY", false, "stop_sequence"},
	}
	for _, c := range cases {
		var body []byte
		if c.hasToolUse {
			body = geminiToolResponse
		} else {
			body = []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}],"role":"model"},"finishReason":"` + c.reason + `"}],"usageMetadata":{}}`)
		}
		out, err := translate.GeminiToAnthropicResponse(body, "m")
		require.NoError(t, err)
		doc := unmarshal(t, out)
		assert.Equal(t, c.expected, doc["stop_reason"],
			"gemini %q hasToolUse=%v → anthropic %q", c.reason, c.hasToolUse, c.expected)
	}
}

func TestGeminiToAnthropicResponse_UsageTranslation(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiTextResponse, "m")
	require.NoError(t, err)
	u, _ := unmarshal(t, out)["usage"].(map[string]any)
	require.NotNil(t, u)
	assert.Equal(t, float64(25), u["input_tokens"])
	assert.Equal(t, float64(10), u["output_tokens"])
}

func TestGeminiToAnthropicResponse_ThoughtSignaturePreserved(t *testing.T) {
	out, err := translate.GeminiToAnthropicResponse(geminiThoughtSigResponse, "m")
	require.NoError(t, err)
	doc := unmarshal(t, out)

	blocks := content(t, doc)
	require.Len(t, blocks, 1)
	blk, _ := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", blk["type"])

	// The signature rides solely in the tool_use id — the one carrier every
	// client SDK round-trips. No off-spec thought_signature field is emitted.
	assert.NotContains(t, blk, "thought_signature", "off-spec field must not be emitted")

	id, _ := blk["id"].(string)
	encoded := base64.RawURLEncoding.EncodeToString([]byte("OPAQUE_SIG_ABC"))
	assert.Contains(t, id, encoded, "signature must be embedded in tool_use ID for round-trip")
}
func TestAnthropicToOpenAIError_WrapsError(t *testing.T) {
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens must be positive"}}`)
	out := translate.AnthropicToOpenAIError(body)
	doc := unmarshal(t, out)

	assert.NotContains(t, doc, "type", "Anthropic top-level type must not leak")
	errObj, _ := doc["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "invalid_request_error", errObj["type"])
	assert.Equal(t, "max_tokens must be positive", errObj["message"])
	assert.Contains(t, errObj, "param")
	assert.Contains(t, errObj, "code")
}

func TestAnthropicToOpenAIError_PassthroughOnEmpty(t *testing.T) {
	body := []byte(`<html>Bad Gateway</html>`)
	out := translate.AnthropicToOpenAIError(body)
	assert.Equal(t, body, out, "malformed input must pass through unchanged")
}

func TestAnthropicToOpenAIError_PassthroughOnEmptyFields(t *testing.T) {
	body := []byte(`{"error":{"type":"","message":""}}`)
	out := translate.AnthropicToOpenAIError(body)
	assert.Equal(t, body, out, "empty type and message must pass through unchanged")
}

func TestOpenAIToAnthropicError_WrapsError(t *testing.T) {
	body := []byte(`{"error":{"type":"rate_limit_error","message":"slow down","param":null,"code":null}}`)
	out := translate.OpenAIToAnthropicError(body)
	doc := unmarshal(t, out)

	assert.Equal(t, "error", doc["type"])
	errObj, _ := doc["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "rate_limit_error", errObj["type"])
	assert.Equal(t, "slow down", errObj["message"])
}

func TestOpenAIToAnthropicError_PassthroughOnEmpty(t *testing.T) {
	body := []byte(`<html>502</html>`)
	out := translate.OpenAIToAnthropicError(body)
	assert.Equal(t, body, out, "malformed input must pass through unchanged")
}

func TestOpenAIToAnthropicError_PassthroughOnEmptyFields(t *testing.T) {
	body := []byte(`{"error":{"type":"","message":""}}`)
	out := translate.OpenAIToAnthropicError(body)
	assert.Equal(t, body, out, "empty type and message must pass through unchanged")
}

func TestGeminiToOpenAIError_WrapsError(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED"}}`)
	out := translate.GeminiToOpenAIError(body)
	doc := unmarshal(t, out)

	errObj, _ := doc["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "resource_exhausted", errObj["type"])
	assert.Equal(t, "Resource has been exhausted", errObj["message"])
	assert.Equal(t, float64(429), errObj["code"])
	assert.Contains(t, errObj, "param")
}

func TestGeminiToOpenAIError_PassthroughOnEmpty(t *testing.T) {
	body := []byte(`<html>503</html>`)
	out := translate.GeminiToOpenAIError(body)
	assert.Equal(t, body, out, "malformed input must pass through unchanged")
}

func TestGeminiToOpenAIError_PassthroughOnEmptyMessageAndStatus(t *testing.T) {
	body := []byte(`{"error":{"code":0,"message":"","status":""}}`)
	out := translate.GeminiToOpenAIError(body)
	assert.Equal(t, body, out, "empty message and status must pass through unchanged")
}

func TestGeminiToOpenAIError_MissingCodeIsZero(t *testing.T) {
	body := []byte(`{"error":{"message":"something broke","status":"INTERNAL"}}`)
	out := translate.GeminiToOpenAIError(body)
	doc := unmarshal(t, out)
	errObj, _ := doc["error"].(map[string]any)
	require.NotNil(t, errObj)
	// gjson.Int() returns 0 for absent fields, matching the struct-based behavior.
	assert.Equal(t, float64(0), errObj["code"])
}
func TestAnthropicToOpenAIResponse_InvalidJSON_ReturnsError(t *testing.T) {
	_, err := translate.AnthropicToOpenAIResponse([]byte(`not json`), "m")
	assert.Error(t, err)
}

func TestOpenAIToAnthropicResponse_InvalidJSON_ReturnsError(t *testing.T) {
	_, err := translate.OpenAIToAnthropicResponse([]byte(`not json`), "m")
	assert.Error(t, err)
}

func TestGeminiToOpenAIResponse_NullArgsNormalizedToEmptyObject(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {"parts": [
				{"functionCall": {"name": "Bash", "args": null}}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5}
	}`)
	out, err := translate.GeminiToOpenAIResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))

	choices, _ := doc["choices"].([]any)
	require.Len(t, choices, 1)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	require.Len(t, tcs, 1)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	assert.Equal(t, "{}", fn["arguments"], "null args must be normalized to empty object string, not \"null\"")
}

func TestGeminiToAnthropicResponse_NullArgsNormalizedToEmptyObject(t *testing.T) {
	body := []byte(`{
		"candidates": [{
			"content": {"parts": [
				{"functionCall": {"name": "Bash", "args": null}}
			]},
			"finishReason": "STOP"
		}],
		"usageMetadata": {"promptTokenCount": 10, "candidatesTokenCount": 5}
	}`)
	out, err := translate.GeminiToAnthropicResponse(body, "gemini-2.5-flash")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out, &doc))

	blocks, _ := doc["content"].([]any)
	require.Len(t, blocks, 1)
	block := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", block["type"])
	input := block["input"]
	require.NotNil(t, input, "input must not be nil/null")
	inputMap, ok := input.(map[string]any)
	require.True(t, ok, "input must be an object, got %T", input)
	assert.Empty(t, inputMap, "null args must be normalized to empty object")
}

func TestGeminiToOpenAIResponse_InvalidJSON_ReturnsError(t *testing.T) {
	_, err := translate.GeminiToOpenAIResponse([]byte(`not json`), "m")
	assert.Error(t, err)
}

func TestGeminiToAnthropicResponse_InvalidJSON_ReturnsError(t *testing.T) {
	_, err := translate.GeminiToAnthropicResponse([]byte(`not json`), "m")
	assert.Error(t, err)
}
