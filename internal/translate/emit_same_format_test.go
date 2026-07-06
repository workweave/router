package translate_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
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

// An off-spec thought_signature field (smuggled from a Gemini turn) 400s an
// Anthropic upstream, so same-format emit must strip it from every block. On
// tool_use the signature still round-trips via the id; on text it's just dropped.
func TestAnthropicSameFormat_StripsThoughtSignature(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[
		{"role":"assistant","content":[
			{"type":"text","text":"thinking out loud","thought_signature":"TEXT_SIG"},
			{"type":"tool_use","id":"call_x__thought__QUFB","name":"Read","input":{"file_path":"main.go"},"thought_signature":"OPAQUE_SIG"}
		]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_x__thought__QUFB","content":"ok"}]}
	]}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-8",
		Capabilities: router.Lookup("claude-opus-4-8"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.NotEmpty(t, msgs)
	asst, _ := msgs[0].(map[string]any)
	content, _ := asst["content"].([]any)
	require.Len(t, content, 2)
	text, _ := content[0].(map[string]any)
	assert.NotContains(t, text, "thought_signature", "Anthropic rejects the off-spec field on text blocks")
	assert.Equal(t, "thinking out loud", text["text"], "the rest of the text block is untouched")
	tool, _ := content[1].(map[string]any)
	assert.NotContains(t, tool, "thought_signature", "Anthropic rejects the off-spec field on tool_use blocks")
	assert.Equal(t, "Read", tool["name"], "the rest of the tool_use block is untouched")
	assert.Equal(t, "call_x__thought__QUFB", tool["id"], "id (signature carrier) survives")
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

func TestAnthropicSameFormat_StripsUnsupportedToolSchemaPattern(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hello"}],"max_tokens":1024,"tools":[{
		"name":"DecimalTool",
		"description":"uses a pydantic decimal schema",
		"input_schema":{
			"type":"object",
			"properties":{
				"amount":{"type":"string","pattern":"^(?!^[-+.]*$)[+-]?0*\\d*\\.?\\d*$","description":"decimal"}
			},
			"required":["amount"]
		}
	}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)

	tools, _ := out["tools"].([]any)
	require.Len(t, tools, 1)
	tool, _ := tools[0].(map[string]any)
	inputSchema, _ := tool["input_schema"].(map[string]any)
	props, _ := inputSchema["properties"].(map[string]any)
	amount, _ := props["amount"].(map[string]any)
	assert.NotContains(t, amount, "pattern", "Anthropic rejects regex lookahead in JSON Schema patterns")
	assert.Equal(t, "string", amount["type"])
	assert.Equal(t, "decimal", amount["description"])
}

func TestOpenAIToAnthropic_StripsUnsupportedToolSchemaPattern(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}],"tools":[{
		"type":"function",
		"function":{
			"name":"DecimalTool",
			"description":"uses a pydantic decimal schema",
			"parameters":{
				"type":"object",
				"properties":{
					"amount":{"type":"string","pattern":"^(?!^[-+.]*$)[+-]?0*\\d*\\.?\\d*$","description":"decimal"}
				},
				"required":["amount"]
			}
		}
	}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, opts)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))

	tools, _ := out["tools"].([]any)
	require.Len(t, tools, 1)
	tool, _ := tools[0].(map[string]any)
	inputSchema, _ := tool["input_schema"].(map[string]any)
	props, _ := inputSchema["properties"].(map[string]any)
	amount, _ := props["amount"].(map[string]any)
	assert.NotContains(t, amount, "pattern", "Anthropic rejects regex lookahead in JSON Schema patterns")
	assert.Equal(t, "string", amount["type"])
	assert.Equal(t, "decimal", amount["description"])
}

func TestAnthropicSameFormat_SystemMessageHoistedWhenNoSystemField(t *testing.T) {
	// A role:"system" message inside the messages array is invalid for the
	// Anthropic Messages API; it must be hoisted to the top-level system field.
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"},{"role":"system","content":"be terse"},{"role":"assistant","content":"ok"}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)

	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2, "system message removed from array")
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		assert.NotEqual(t, "system", mm["role"], "no system role left in messages")
	}
	sys, _ := out["system"].([]any)
	require.Len(t, sys, 1)
	block, _ := sys[0].(map[string]any)
	assert.Equal(t, "text", block["type"])
	assert.Equal(t, "be terse", block["text"])
}

func TestAnthropicSameFormat_SystemMessageMergedWithExistingSystem(t *testing.T) {
	// Existing top-level system is preserved and the hoisted text appended.
	body := []byte(`{"model":"claude-sonnet-4-20250514","system":"top-level rules","messages":[{"role":"system","content":[{"type":"text","text":"extra rule"}]},{"role":"user","content":"hi"}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)

	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 1)
	sys, _ := out["system"].([]any)
	require.Len(t, sys, 2, "existing system block kept, hoisted block appended")
	first, _ := sys[0].(map[string]any)
	second, _ := sys[1].(map[string]any)
	assert.Equal(t, "top-level rules", first["text"])
	assert.Equal(t, "extra rule", second["text"])
}

func TestAnthropicSameFormat_NoSystemMessageIsNoOp(t *testing.T) {
	// Common case: no in-array system message — body passes through unchanged.
	body := []byte(`{"model":"claude-sonnet-4-20250514","system":"rules","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, "rules", out["system"], "untouched string system field")
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
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"thought"},{"type":"text","text":"reply"}]}],"max_tokens":1024,"thinking":{"type":"adaptive"}}`)
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

// Legacy clients still send thinking.type=enabled, which claude-opus-4-6+
// rejects (400). The router must upconvert to type=adaptive and derive
// output_config.effort from budget_tokens (prod incident 2026-06-02).
func TestAnthropicSameFormat_EnabledThinkingUpconvertedToAdaptive(t *testing.T) {
	tests := []struct {
		name         string
		budgetTokens int
		wantEffort   string
	}{
		{"low budget", 2048, "low"},
		{"medium budget (pi default)", 8192, "medium"},
		{"high budget", 32000, "high"},
		{"missing budget defaults to medium", 0, "medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thinkingField := `"thinking":{"type":"enabled"}`
			if tc.budgetTokens > 0 {
				thinkingField = fmt.Sprintf(`"thinking":{"type":"enabled","budget_tokens":%d}`, tc.budgetTokens)
			}
			body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,` + thinkingField + `}`)
			opts := translate.EmitOptions{
				TargetModel:  "claude-opus-4-7",
				Capabilities: router.Lookup("claude-opus-4-7"),
			}
			out := parseAndEmit(t, body, "anthropic", opts)

			thinking, _ := out["thinking"].(map[string]any)
			require.NotNil(t, thinking, "thinking should remain on the body")
			assert.Equal(t, "adaptive", thinking["type"], "legacy enabled must be upconverted to adaptive")
			_, hasBudget := thinking["budget_tokens"]
			assert.False(t, hasBudget, "budget_tokens has no meaning under adaptive thinking and must be dropped")

			outputConfig, _ := out["output_config"].(map[string]any)
			require.NotNil(t, outputConfig, "output_config.effort is required when adaptive thinking is set")
			assert.Equal(t, tc.wantEffort, outputConfig["effort"])
		})
	}
}

// An explicit output_config.effort wins over the budget-derived default.
func TestAnthropicSameFormat_EnabledThinkingPreservesExplicitEffort(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":8192},"output_config":{"effort":"high"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	outputConfig, _ := out["output_config"].(map[string]any)
	require.NotNil(t, outputConfig)
	assert.Equal(t, "high", outputConfig["effort"], "caller-supplied effort must not be overwritten")
}

func TestAnthropicSameFormat_ThinkingBlocksStrippedOnModelSwitch(t *testing.T) {
	// On a model switch, prior thinking blocks carry signatures from the old
	// model; Anthropic 400s on those. ModelSwitched forces the strip.
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"sig-from-other-model"},{"type":"text","text":"reply"}]}],"max_tokens":1024,"thinking":{"type":"adaptive"}}`)
	opts := translate.EmitOptions{
		TargetModel:   "claude-opus-4-7",
		Capabilities:  router.Lookup("claude-opus-4-7"),
		ModelSwitched: true,
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 1, "thinking block with stale signature must be stripped on a model switch")
	block, _ := content[0].(map[string]any)
	assert.Equal(t, "text", block["type"], "only the text block should survive the strip")
}

func TestAnthropicSameFormat_ThinkingBlocksKeptWhenNoModelSwitch(t *testing.T) {
	// Same-model turn (ModelSwitched=false): valid thinking blocks must survive
	// to keep prompt-cache hits and reasoning continuity intact.
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"thought","signature":"valid-sig"},{"type":"text","text":"reply"}]}],"max_tokens":1024,"thinking":{"type":"adaptive"}}`)
	opts := translate.EmitOptions{
		TargetModel:   "claude-opus-4-7",
		Capabilities:  router.Lookup("claude-opus-4-7"),
		ModelSwitched: false,
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	msgs, _ := out["messages"].([]any)
	require.Len(t, msgs, 2)
	assistantMsg, _ := msgs[1].(map[string]any)
	content, _ := assistantMsg["content"].([]any)
	require.Len(t, content, 2, "thinking blocks must be preserved when the model did not change")
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

// Prod repro (2026-06-09): a session using effort="xhigh" was re-routed
// mid-session to claude-sonnet-4-6 (tops out at "max"), and Anthropic
// 400'd on the unsupported level, killing the session. Emit must clamp
// xhigh to max for adaptive targets without CapXhighEffort.
func TestAnthropicSameFormat_XhighEffortClampedOnReroute(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-sonnet-4-6",
		Capabilities: router.Lookup("claude-sonnet-4-6"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	outputConfig, _ := out["output_config"].(map[string]any)
	require.NotNil(t, outputConfig)
	assert.Equal(t, "max", outputConfig["effort"], "xhigh must clamp to max for models without CapXhighEffort")
}

// Top-level `effort` follows the same menu as output_config.effort.
func TestAnthropicSameFormat_XhighTopLevelEffortClampedOnReroute(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"},"effort":"xhigh"}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-sonnet-4-6",
		Capabilities: router.Lookup("claude-sonnet-4-6"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, "max", out["effort"], "top-level xhigh must clamp to max for models without CapXhighEffort")
}

// xhigh stays untouched when the target supports it (opus-4-7+).
func TestAnthropicSameFormat_XhighEffortPreservedForCapableModel(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"xhigh"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-8",
		Capabilities: router.Lookup("claude-opus-4-8"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	outputConfig, _ := out["output_config"].(map[string]any)
	require.NotNil(t, outputConfig)
	assert.Equal(t, "xhigh", outputConfig["effort"], "xhigh must pass through to models with CapXhighEffort")
}

// Exhaustive backstop for the 2026-06-09 incident: a newly added or re-tagged
// Anthropic model could silently reintroduce the 400. Walk every catalog model
// and assert xhigh survives emit only when CapXhighEffort is advertised.
func TestAnthropicSameFormat_XhighEffortNeverReachesIncapableModel(t *testing.T) {
	var anthropicModels, capableModels int
	for _, m := range catalog.Models {
		if m.PrimaryProvider() != providers.ProviderAnthropic {
			continue
		}
		anthropicModels++
		capable := router.Lookup(m.ID).Supports(router.CapXhighEffort)
		if capable {
			capableModels++
		}
		t.Run(m.ID, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"},"effort":"xhigh","output_config":{"effort":"xhigh"}}`)
			out := parseAndEmit(t, body, "anthropic", translate.EmitOptions{
				TargetModel:  m.ID,
				Capabilities: router.Lookup(m.ID),
			})

			topLevel, _ := out["effort"].(string)
			var nested string
			if oc, ok := out["output_config"].(map[string]any); ok {
				nested, _ = oc["effort"].(string)
			}
			if capable {
				assert.Equal(t, "xhigh", topLevel, "top-level effort xhigh must pass through to a CapXhighEffort model")
				assert.Equal(t, "xhigh", nested, "output_config.effort xhigh must pass through to a CapXhighEffort model")
				return
			}
			assert.NotEqual(t, "xhigh", topLevel, "top-level effort xhigh must never reach a model without CapXhighEffort")
			assert.NotEqual(t, "xhigh", nested, "output_config.effort xhigh must never reach a model without CapXhighEffort")
		})
	}
	// Guard against a vacuous pass if the catalog filter ever stops matching.
	require.Positive(t, anthropicModels, "expected Anthropic models in the catalog")
	require.Positive(t, capableModels, "expected at least one CapXhighEffort model so the preserve branch is exercised")
}

// Levels below xhigh are on every adaptive model's menu; the clamp must not
// touch them even when the target lacks CapXhighEffort.
func TestAnthropicSameFormat_NonXhighEffortUntouchedByClamp(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":1024,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-sonnet-4-6",
		Capabilities: router.Lookup("claude-sonnet-4-6"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	outputConfig, _ := out["output_config"].(map[string]any)
	require.NotNil(t, outputConfig)
	assert.Equal(t, "high", outputConfig["effort"], "supported effort levels must pass through unchanged")
}
