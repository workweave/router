package translate_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseAndEmit(t *testing.T, body []byte, format string, opts translate.EmitOptions) map[string]any {
	t.Helper()
	switch format {
	case "openai":
		env, parseErr := translate.ParseOpenAI(body)
		require.NoError(t, parseErr)
		p, emitErr := env.PrepareOpenAI(http.Header{}, opts)
		require.NoError(t, emitErr)
		var out map[string]any
		require.NoError(t, json.Unmarshal(p.Body, &out))
		return out
	case "anthropic":
		env, parseErr := translate.ParseAnthropic(body)
		require.NoError(t, parseErr)
		p, emitErr := env.PrepareAnthropic(http.Header{}, opts)
		require.NoError(t, emitErr)
		var out map[string]any
		require.NoError(t, json.Unmarshal(p.Body, &out))
		return out
	default:
		t.Fatalf("unknown format: %s", format)
		return nil
	}
}

func TestOpenAISameFormat_ModelRewrite(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, "gpt-4.1", out["model"])
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 1)
	msg, _ := msgs[0].(map[string]any)
	assert.Equal(t, "user", msg["role"])
	assert.Equal(t, "hello", msg["content"])
	assert.Equal(t, true, out["stream"])
}

func TestOpenAISameFormat_UnknownFieldsPreserved(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"custom_field":"preserved","metadata":{"key":"val"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, "preserved", out["custom_field"])
	meta, _ := out["metadata"].(map[string]any)
	assert.Equal(t, "val", meta["key"])
}

func TestOpenAISameFormat_ThinkingDeleted(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.NotContains(t, out, "thinking")
}

func TestOpenAISameFormat_MaxTokensRenamedForReasoning(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":500}`)
	opts := translate.EmitOptions{
		TargetModel:  "o3",
		Capabilities: router.Lookup("o3"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.NotContains(t, out, "max_tokens")
	assert.Equal(t, float64(500), out["max_completion_tokens"])
}

func TestOpenAISameFormat_MaxTokensNotRenamedWhenCompTokensAlreadySet(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":500,"max_completion_tokens":1000}`)
	opts := translate.EmitOptions{
		TargetModel:  "o3",
		Capabilities: router.Lookup("o3"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.NotContains(t, out, "max_tokens")
	assert.Equal(t, float64(1000), out["max_completion_tokens"])
}

func TestOpenAISameFormat_ReasoningEffortDeletedForNonReasoning(t *testing.T) {
	body := []byte(`{"model":"o3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.NotContains(t, out, "reasoning_effort")
}

func TestOpenAISameFormat_ReasoningEffortKeptForReasoning(t *testing.T) {
	body := []byte(`{"model":"o3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	opts := translate.EmitOptions{
		TargetModel:  "o3-mini",
		Capabilities: router.Lookup("o3-mini"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, "high", out["reasoning_effort"])
}

func TestOpenAISameFormat_StreamUsageInjected(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	opts := translate.EmitOptions{
		TargetModel:        "gpt-4.1",
		Capabilities:       router.Lookup("gpt-4.1"),
		IncludeStreamUsage: true,
	}
	out := parseAndEmit(t, body, "openai", opts)
	so, _ := out["stream_options"].(map[string]any)
	require.NotNil(t, so)
	assert.Equal(t, true, so["include_usage"])
}

func TestOpenAISameFormat_StreamUsageNotInjectedForNonStreaming(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:        "gpt-4.1",
		Capabilities:       router.Lookup("gpt-4.1"),
		IncludeStreamUsage: true,
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.NotContains(t, out, "stream_options")
}

func TestOpenAISameFormat_OutputTokensClamped(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":999999}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4o",
		Capabilities: router.Lookup("gpt-4o"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.LessOrEqual(t, out["max_tokens"].(float64), float64(16384))
}

func TestAnthropicSameFormat_ModelRewrite(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, "claude-opus-4-7", out["model"])
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 1)
}

func TestAnthropicSameFormat_UnknownFieldsPreserved(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"custom_field":"preserved","metadata":{"key":"val"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, "preserved", out["custom_field"])
	meta, _ := out["metadata"].(map[string]any)
	assert.Equal(t, "val", meta["key"])
}

func TestAnthropicSameFormat_ThinkingStrippedForNonThinkingModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":5000}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.NotContains(t, out, "thinking")
}

func TestAnthropicSameFormat_AdaptiveThinkingKeptForCapableModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	thinking, _ := out["thinking"].(map[string]any)
	require.NotNil(t, thinking, "thinking should be preserved for models with adaptive thinking capability")
	assert.Equal(t, "adaptive", thinking["type"])
}

func TestAnthropicSameFormat_ContextManagementDeletedForNonAdaptive(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"context_management":{"mode":"auto"},"effort":"high","output_config":{"length":"verbose"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.NotContains(t, out, "context_management")
	assert.NotContains(t, out, "effort")
	assert.NotContains(t, out, "output_config")
}

func TestAnthropicSameFormat_ThinkingBlocksFiltered(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"internal thought"},{"type":"text","text":"visible reply"}]}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 1, "thinking block should be filtered out")
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "visible reply", block["text"])
}

func TestAnthropicSameFormat_RedactedThinkingBlocksFiltered(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"redacted_thinking","data":"abc"},{"type":"text","text":"reply"}]}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 1)
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
}

func TestAnthropicSameFormat_ThinkingBlocksKeptForCapableModel(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"thought"},{"type":"text","text":"reply"}]}],"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":5000}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 2, "thinking blocks should be preserved for capable models")
}

func TestPassthroughSameFormat_FieldsScrubbed(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"effort":"high","thinking":{"type":"enabled"},"context_management":{"mode":"auto"},"output_config":{"length":"verbose"}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropicPassthrough(http.Header{})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.NotContains(t, out, "effort")
	assert.NotContains(t, out, "thinking")
	assert.NotContains(t, out, "context_management")
	assert.NotContains(t, out, "output_config")
	assert.Equal(t, "claude-sonnet-4-20250514", out["model"])
	assert.Contains(t, out, "messages")
	assert.Contains(t, out, "max_tokens")
}

func TestPassthroughSameFormat_NoScrubNeeded(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropicPassthrough(http.Header{})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, "claude-sonnet-4-20250514", out["model"])
}

func TestOpenAISameFormat_ComplexRequestPreservesStructure(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "Thanks!"}
		],
		"temperature": 0.7,
		"top_p": 0.9,
		"stream": true,
		"tools": [{"type":"function","function":{"name":"calc","description":"Calculate","parameters":{"type":"object","properties":{"expr":{"type":"string"}}}}}],
		"n": 1,
		"presence_penalty": 0.5
	}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, "gpt-4.1", out["model"])
	assert.Equal(t, 0.7, out["temperature"])
	assert.Equal(t, 0.9, out["top_p"])
	assert.Equal(t, true, out["stream"])
	assert.Equal(t, float64(1), out["n"])
	assert.Equal(t, 0.5, out["presence_penalty"])
	msgs, _ := out["messages"].([]any)
	assert.Len(t, msgs, 4)
	tools, _ := out["tools"].([]any)
	assert.Len(t, tools, 1)
}

func TestAnthropicSameFormat_MultipleThinkingBlocksAcrossMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": "first"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "thought1"},
				{"type": "text", "text": "reply1"}
			]},
			{"role": "user", "content": "second"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "thought2"},
				{"type": "redacted_thinking", "data": "xyz"},
				{"type": "text", "text": "reply2"}
			]}
		],
		"max_tokens": 1024
	}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 4)

	msg1, _ := msgs[1].(map[string]any)
	content1, _ := msg1["content"].([]any)
	require.Len(t, content1, 1)
	assert.Equal(t, "text", content1[0].(map[string]any)["type"])

	msg3, _ := msgs[3].(map[string]any)
	content3, _ := msg3["content"].([]any)
	require.Len(t, content3, 1)
	assert.Equal(t, "text", content3[0].(map[string]any)["type"])
}

func TestAnthropicSameFormat_ManyThinkingBlocksInSingleMessage(t *testing.T) {
	var blocks []string
	for i := 0; i < 200; i++ {
		blocks = append(blocks, `{"type":"thinking","thinking":"t"}`)
	}
	blocks = append(blocks, `{"type":"text","text":"final"}`)
	contentArray := "[" + joinStrings(blocks, ",") + "]"
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":` + contentArray + `}],"max_tokens":1024}`)

	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 1, "all 200 thinking blocks should be stripped, leaving only the text block")
	assert.Equal(t, "text", content[0].(map[string]any)["type"])
	assert.Equal(t, "final", content[0].(map[string]any)["text"])
}

func TestAnthropicSameFormat_AllThinkingBlocksRemoved(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"t1"},{"type":"redacted_thinking","data":"xyz"}]}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	assert.Len(t, content, 0, "all blocks are thinking blocks, content should be empty array")
}

func TestAnthropicSameFormat_StringContentPreserved(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"world"},{"role":"user","content":"again"},{"role":"assistant","content":[{"type":"thinking","thinking":"t"},{"type":"text","text":"reply"}]}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-3-haiku-20240307",
		Capabilities: router.Lookup("claude-3-haiku-20240307"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 4)

	msg1, _ := msgs[1].(map[string]any)
	assert.Equal(t, "world", msg1["content"], "string content should pass through unchanged")

	msg3, _ := msgs[3].(map[string]any)
	content, _ := msg3["content"].([]any)
	require.Len(t, content, 1)
	assert.Equal(t, "text", content[0].(map[string]any)["type"])
}

func joinStrings(elems []string, sep string) string {
	result := ""
	for i, e := range elems {
		if i > 0 {
			result += sep
		}
		result += e
	}
	return result
}

func TestOpenAISameFormat_ClampMaxCompletionTokens(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":999999}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4o",
		Capabilities: router.Lookup("gpt-4o"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.LessOrEqual(t, out["max_completion_tokens"].(float64), float64(16384))
}

func TestOpenAISameFormat_StreamUsagePreservesExistingOptions(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"something":"custom"}}`)
	opts := translate.EmitOptions{
		TargetModel:        "gpt-4.1",
		Capabilities:       router.Lookup("gpt-4.1"),
		IncludeStreamUsage: true,
	}
	out := parseAndEmit(t, body, "openai", opts)
	so, _ := out["stream_options"].(map[string]any)
	require.NotNil(t, so)
	assert.Equal(t, true, so["include_usage"])
	assert.Equal(t, "custom", so["something"])
}

func TestAnthropicSameFormat_BodyIsImmutable(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`)
	original := make([]byte, len(body))
	copy(original, body)

	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	_, err = env.PrepareAnthropic(http.Header{}, opts)
	require.NoError(t, err)

	assert.Equal(t, original, body, "original body bytes must not be modified")
}
