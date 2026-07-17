package translate_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func itoa(n int) string { return strconv.Itoa(n) }

func gjsonStopReason(b []byte) string {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s, _ := m["stop_reason"].(string)
	return s
}

func openAIReasoningTestSignature(t *testing.T, id, enc string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"v": 1, "provider": "openai", "id": id, "enc": enc})
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

func decodeOpenAIReasoningTestSignature(t *testing.T, sig string) map[string]any {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(sig)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// Anthropic → OpenAI Responses request shape: thinking budget, typed input
// items, tool_use/tool_result mapping, flat tools.
func TestPrepareOpenAIResponses_RequestShape(t *testing.T) {
	body := []byte(`{
      "model":"claude-opus-4-8","max_tokens":4096,
      "system":"You are helpful.",
      "thinking":{"type":"enabled","budget_tokens":31999},
      "tools":[{"name":"bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
      "tool_choice":{"type":"auto"},
      "messages":[
        {"role":"user","content":"fix the bug"},
        {"role":"assistant","content":[
          {"type":"text","text":"I'll look"},
          {"type":"tool_use","id":"toolu_1","name":"bash","input":{"command":"ls"}}
        ]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file.go"}]}
      ]
    }`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
		TargetModel:          "gpt-5.5",
		Capabilities:         router.Lookup("gpt-5.5"),
		ForceReasoningEffort: "high",
	})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))

	assert.Equal(t, "gpt-5.5", out["model"])
	assert.Equal(t, true, out["stream"], "Responses upstream must stream so a slow gpt-5.x prefill doesn't trip the response-header timeout")
	assert.Equal(t, false, out["store"])
	assert.Equal(t, "You are helpful.", out["instructions"])
	reasoning, _ := out["reasoning"].(map[string]any)
	require.NotNil(t, reasoning, "reasoning must be set from thinking budget")
	assert.Equal(t, "high", reasoning["effort"], "31999 budget -> high")
	// max_tokens 4096 is floored to minResponsesOutputTokens (16000): a reasoning
	// model needs output-budget headroom for hidden reasoning before any visible
	// token, so the requested 4096 is lifted to the reasoning floor.
	assert.EqualValues(t, 16000, out["max_output_tokens"])
	assert.Equal(t, "auto", out["tool_choice"])

	// tools: FLAT function shape (no nested "function" wrapper)
	tools, _ := out["tools"].([]any)
	require.Len(t, tools, 1)
	tool0, _ := tools[0].(map[string]any)
	assert.Equal(t, "function", tool0["type"])
	assert.Equal(t, "bash", tool0["name"])
	assert.Nil(t, tool0["function"], "Responses tools are flat, not nested under function")
	require.NotNil(t, tool0["parameters"])

	// input items in order: user message, assistant message (text), function_call, function_call_output
	input, _ := out["input"].([]any)
	require.GreaterOrEqual(t, len(input), 4)
	types := make([]string, 0, len(input))
	var fc, fco map[string]any
	for _, it := range input {
		m, _ := it.(map[string]any)
		if r, ok := m["role"]; ok {
			types = append(types, "msg:"+r.(string))
			continue
		}
		switch m["type"] {
		case "function_call":
			fc = m
			types = append(types, "function_call")
		case "function_call_output":
			fco = m
			types = append(types, "function_call_output")
		}
	}
	assert.Equal(t, []string{"msg:user", "msg:assistant", "function_call", "function_call_output"}, types)
	require.NotNil(t, fc)
	assert.Equal(t, "toolu_1", fc["call_id"], "tool_use.id must round-trip as call_id")
	assert.Equal(t, "bash", fc["name"])
	assert.Equal(t, `{"command":"ls"}`, fc["arguments"], "arguments serialized as a JSON string")
	require.NotNil(t, fco)
	assert.Equal(t, "toolu_1", fco["call_id"], "tool_result.tool_use_id must match the call_id")
	assert.Equal(t, "file.go", fco["output"])

	assert.Equal(t, providers.EndpointResponses, prep.Endpoint)
}

// TestPrepareOpenAIResponses_ToolChoiceVariants covers the Anthropic ->
// Responses tool_choice mapping for "any" and named-tool; "auto" is covered
// by TestPrepareOpenAIResponses_RequestShape.
func TestPrepareOpenAIResponses_ToolChoiceVariants(t *testing.T) {
	cases := []struct {
		name       string
		toolChoice string
		want       any
	}{
		{"any", `{"type":"any"}`, "required"},
		{"tool", `{"type":"tool","name":"bash"}`, map[string]any{"type": "function", "name": "bash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{
				"model":"claude-opus-4-8","max_tokens":4096,
				"tools":[{"name":"bash","input_schema":{"type":"object"}}],
				"tool_choice":` + tc.toolChoice + `,
				"messages":[{"role":"user","content":"fix the bug"}]
			}`)
			env, err := translate.ParseAnthropic(body)
			require.NoError(t, err)
			prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
				TargetModel:  "gpt-5.5",
				Capabilities: router.Lookup("gpt-5.5"),
			})
			require.NoError(t, err)

			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			assert.Equal(t, tc.want, out["tool_choice"])
		})
	}
}

// A tiny client max_tokens (Claude Code sends 1 for a probe, 64 for a
// title/topic turn) must be floored to the reasoning output budget: a reasoning
// model burns the budget on hidden reasoning before any visible token, so the
// raw tiny value 400s ("max_tokens or model output limit was reached"). A budget
// already above the floor is passed through unchanged (clamped only to the cap).
func TestPrepareOpenAIResponses_FloorsMaxOutputTokensForReasoning(t *testing.T) {
	cases := []struct {
		name       string
		maxTokens  int
		wantMaxOut int64
	}{
		{name: "probe max_tokens=1 floored", maxTokens: 1, wantMaxOut: 16000},
		{name: "title-turn max_tokens=64 floored", maxTokens: 64, wantMaxOut: 16000},
		{name: "at floor unchanged", maxTokens: 16000, wantMaxOut: 16000},
		{name: "large budget passes through", maxTokens: 32000, wantMaxOut: 32000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{
				"model":"claude-opus-4-8","max_tokens":%d,
				"tools":[{"name":"bash","description":"run","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
				"messages":[{"role":"user","content":"hi"}]
			}`, tc.maxTokens))
			env, err := translate.ParseAnthropic(body)
			require.NoError(t, err)
			prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
				TargetModel:  "gpt-5.4-mini",
				Capabilities: router.Lookup("gpt-5.4-mini"),
			})
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			assert.EqualValues(t, tc.wantMaxOut, out["max_output_tokens"])
		})
	}
}

// Gemini sessions smuggle a thoughtSignature into tool_use ids
// (call_xxx__thought__<sig>, often >1KB). On re-route to a gpt-5.x Responses
// model, call_id must be stripped and clamped to 64 chars or the upstream
// 400s; tool_use and tool_result must still share the clamped call_id.
func TestPrepareOpenAIResponses_ClampsGeminiThoughtSignatureCallID(t *testing.T) {
	longSig := strings.Repeat("A", 1300) // valid base64url, > 64 chars
	id := "call_abc123__thought__" + longSig
	require.Greater(t, len(id), 1300)
	body := []byte(`{
		"model":"claude-opus-4-8","max_tokens":1024,
		"messages":[
			{"role":"user","content":"continue"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":` + strconv.Quote(id) + `,"name":"Read","input":{"file_path":"main.go"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":` + strconv.Quote(id) + `,"content":"ok"}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5"), ForceReasoningEffort: "high"})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	input, _ := out["input"].([]any)
	var fnCallID, fnOutCallID string
	for _, item := range input {
		m, _ := item.(map[string]any)
		switch m["type"] {
		case "function_call":
			fnCallID, _ = m["call_id"].(string)
		case "function_call_output":
			fnOutCallID, _ = m["call_id"].(string)
		}
	}
	require.NotEmpty(t, fnCallID)
	require.NotEmpty(t, fnOutCallID)
	assert.LessOrEqual(t, len(fnCallID), 64, "call_id must fit OpenAI's 64-char limit")
	assert.Equal(t, "call_abc123", fnCallID, "the bare id (sans __thought__) is within the limit")
	assert.Equal(t, fnCallID, fnOutCallID, "tool_use and tool_result must share the clamped call_id")
}

func TestPrepareOpenAIResponses_ReplaysSignedReasoning(t *testing.T) {
	sig := openAIReasoningTestSignature(t, "rs_prev", "enc_prev")
	body := []byte(`{
		"model":"claude-opus-4-8","max_tokens":1024,
		"thinking":{"type":"enabled","budget_tokens":8192},
		"messages":[
			{"role":"user","content":"continue"},
			{"role":"assistant","content":[
				{"type":"text","text":"I'll inspect it."},
				{"type":"thinking","thinking":"summary","signature":` + strconv.Quote(sig) + `},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"main.go"}}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5"), ForceReasoningEffort: "high"})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, []any{"reasoning.encrypted_content"}, out["include"])

	input, _ := out["input"].([]any)
	require.Len(t, input, 4)
	reasoning, _ := input[2].(map[string]any)
	assert.Equal(t, "reasoning", reasoning["type"])
	assert.Equal(t, "rs_prev", reasoning["id"])
	assert.Equal(t, "enc_prev", reasoning["encrypted_content"])
	assert.Equal(t, []any{}, reasoning["summary"])
	toolCall, _ := input[3].(map[string]any)
	assert.Equal(t, "function_call", toolCall["type"])
	assert.Equal(t, "toolu_1", toolCall["call_id"])
}

func TestPrepareOpenAIResponses_ReplaysSignedReasoningAfterModelSwitch(t *testing.T) {
	sig := openAIReasoningTestSignature(t, "rs_prev", "enc_prev")
	body := []byte(`{
		"model":"claude-opus-4-8","max_tokens":1024,
		"thinking":{"type":"enabled","budget_tokens":8192},
		"messages":[
			{"role":"user","content":"continue"},
			{"role":"assistant","content":[
				{"type":"text","text":"I'll inspect it."},
				{"type":"thinking","thinking":"stale anthropic reasoning","signature":"sig-from-other-model"},
				{"type":"thinking","thinking":"summary","signature":` + strconv.Quote(sig) + `},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"main.go"}}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
		TargetModel:          "gpt-5.5",
		Capabilities:         router.Lookup("gpt-5.5"),
		ModelSwitched:        true,
		ForceReasoningEffort: "high",
	})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	input, _ := out["input"].([]any)
	require.Len(t, input, 4)
	reasoning, _ := input[2].(map[string]any)
	assert.Equal(t, "reasoning", reasoning["type"])
	assert.Equal(t, "rs_prev", reasoning["id"])
	assert.Equal(t, "enc_prev", reasoning["encrypted_content"])
	toolCall, _ := input[3].(map[string]any)
	assert.Equal(t, "toolu_1", toolCall["call_id"])
}

// Explicit levels retain their client-selected value; GPT-5 no longer promotes
// medium to high during translation.
func TestPrepareOpenAIResponses_EffortLadder(t *testing.T) {
	for _, tc := range []struct {
		level string
	}{{"low"}, {"medium"}, {"high"}} {
		body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"` + tc.level + `"}`)
		env, err := translate.ParseAnthropic(body)
		require.NoError(t, err)
		prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(prep.Body, &out))
		reasoning, _ := out["reasoning"].(map[string]any)
		require.NotNil(t, reasoning, "level %s", tc.level)
		assert.Equal(t, tc.level, reasoning["effort"])
	}
}

// Responses `response` object → Anthropic message.
func TestResponsesToAnthropicResponse(t *testing.T) {
	body := []byte(`{
      "id":"resp_abc","status":"completed","model":"gpt-5.5",
      "output":[
        {"type":"reasoning","id":"rs_1","encrypted_content":"enc_1","summary":[{"type":"summary_text","text":"thinking about it"}]},
        {"type":"message","id":"m1","role":"assistant","content":[{"type":"output_text","text":"here is the fix"}]},
        {"type":"function_call","id":"fc1","call_id":"call_9","name":"bash","arguments":"{\"command\":\"go test\"}"}
      ],
      "usage":{"input_tokens":1200,"output_tokens":340,"output_tokens_details":{"reasoning_tokens":256},"input_tokens_details":{"cached_tokens":800}}
    }`)
	out, err := translate.ResponsesToAnthropicResponse(body, "gpt-5.5")
	require.NoError(t, err)
	var msg map[string]any
	require.NoError(t, json.Unmarshal(out, &msg))

	assert.Equal(t, "message", msg["type"])
	assert.Equal(t, "resp_abc", msg["id"], "upstream response id passes through as the message id")
	assert.Equal(t, "tool_use", msg["stop_reason"], "a function_call output → stop_reason tool_use")
	content, _ := msg["content"].([]any)
	require.Len(t, content, 3)
	b0, _ := content[0].(map[string]any)
	assert.Equal(t, "thinking", b0["type"])
	assert.Equal(t, "thinking about it", b0["thinking"])
	sigEnv := decodeOpenAIReasoningTestSignature(t, b0["signature"].(string))
	assert.Equal(t, float64(1), sigEnv["v"])
	assert.Equal(t, "openai", sigEnv["provider"])
	assert.Equal(t, "rs_1", sigEnv["id"])
	assert.Equal(t, "enc_1", sigEnv["enc"])
	b1, _ := content[1].(map[string]any)
	assert.Equal(t, "text", b1["type"])
	assert.Equal(t, "here is the fix", b1["text"])
	b2, _ := content[2].(map[string]any)
	assert.Equal(t, "tool_use", b2["type"])
	// The tool_use id also carries the preceding reasoning item's signature,
	// since Claude Code drops the thinking block on round-trip but keeps the id.
	toolID, _ := b2["id"].(string)
	assert.True(t, strings.HasPrefix(toolID, "call_9"), "tool id keeps the call_id prefix, got %q", toolID)
	assert.Contains(t, toolID, "__openai_reasoning__", "tool id carries the reasoning signature for replay")
	assert.Equal(t, "bash", b2["name"])
	input, _ := b2["input"].(map[string]any)
	assert.Equal(t, "go test", input["command"], "arguments string parsed back to an input object")
	usage, _ := msg["usage"].(map[string]any)
	assert.EqualValues(t, 1200, usage["input_tokens"])
	assert.EqualValues(t, 340, usage["output_tokens"])
	assert.EqualValues(t, 800, usage["cache_read_input_tokens"])
}

func TestResponsesToAnthropicResponse_StopReasons(t *testing.T) {
	// max tokens
	mx := []byte(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	out, err := translate.ResponsesToAnthropicResponse(mx, "gpt-5.5")
	require.NoError(t, err)
	assert.Equal(t, "max_tokens", gjsonStopReason(out))
	// plain end_turn
	et := []byte(`{"id":"r","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":2}}`)
	out, err = translate.ResponsesToAnthropicResponse(et, "gpt-5.5")
	require.NoError(t, err)
	assert.Equal(t, "end_turn", gjsonStopReason(out))
	filtered := []byte(`{"id":"r","status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`)
	_, err = translate.ResponsesToAnthropicResponse(filtered, "gpt-5.5")
	require.Error(t, err)
}

// gemini-3.x uses string `thinkingLevel`, not the legacy numeric `thinkingBudget`
// (sending both 400s).
func TestPrepareGemini_ThinkingBudgetToThinkingConfig(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	_, err = env.PrepareGemini(nil, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview", Capabilities: router.Lookup("gemini-3.1-pro-preview")})
	require.ErrorIs(t, err, translate.ErrReasoningIncompatible)
}

// gemini-2.5 (legacy) keeps the numeric thinkingBudget — thinkingLevel is 3.x only.
func TestPrepareGemini_ThinkingBudget_Legacy25(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(nil, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	gen, _ := out["generationConfig"].(map[string]any)
	tc, ok := gen["thinkingConfig"].(map[string]any)
	require.True(t, ok, "thinkingConfig set from thinking budget")
	assert.EqualValues(t, 31999, tc["thinkingBudget"], "budget must be preserved exactly")
	_, hasLevel := tc["thinkingLevel"]
	assert.False(t, hasLevel, "gemini-2.5 must NOT send thinkingLevel")
}

func TestUseOpenAIResponsesAPI(t *testing.T) {
	caps := router.Lookup("gpt-5.4-mini")
	assert.True(t, translate.UseOpenAIResponsesAPI(providers.ProviderOpenAI, caps, true))
	assert.False(t, translate.UseOpenAIResponsesAPI(providers.ProviderOpenAI, caps, false))
	assert.False(t, translate.UseOpenAIResponsesAPI(providers.ProviderFireworks, caps, true))
	assert.False(t, translate.UseOpenAIResponsesAPI(providers.ProviderOpenAI, router.Lookup("gpt-4o"), true))
}
