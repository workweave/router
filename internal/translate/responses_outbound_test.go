package translate_test

import (
	"encoding/base64"
	"encoding/json"
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

// Anthropic (Claude Code) → OpenAI Responses request: the thinking budget must
// become reasoning.effort, messages must become typed input items, tool_use →
// function_call, tool_result → function_call_output, tools the flat shape.
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
		TargetModel:  "gpt-5.5",
		Capabilities: router.Lookup("gpt-5.5"),
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
	assert.EqualValues(t, 4096, out["max_output_tokens"])
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

// A session that ran on Gemini accumulates tool_use ids with a base64
// thoughtSignature smuggled in (call_xxx__thought__<sig>, often >1KB). When a
// later turn re-routes to a gpt-5.x Responses model, the call_id must be
// stripped of the signature and clamped to OpenAI's 64-char limit, or the
// upstream 400s ("input[N].call_id: string too long, max 64"). The tool_use
// and its tool_result must still map to the same clamped call_id so they pair.
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
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
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
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
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
		TargetModel:   "gpt-5.5",
		Capabilities:  router.Lookup("gpt-5.5"),
		ModelSwitched: true,
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

// budget→effort ladder.
func TestPrepareOpenAIResponses_EffortLadder(t *testing.T) {
	// gpt-5.x has a measured "medium" dead-zone on hard agentic coding (Pro:
	// low 16%, medium 0%, high 41%), so the medium band (budget ≤16384) is
	// promoted to high. Small budgets still resolve to low — easy stays cheap.
	for _, tc := range []struct {
		budget int
		want   string
	}{{2048, "low"}, {8192, "high"}, {16384, "high"}, {31999, "high"}} {
		body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":` + itoa(tc.budget) + `}}`)
		env, err := translate.ParseAnthropic(body)
		require.NoError(t, err)
		prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.5", Capabilities: router.Lookup("gpt-5.5")})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(prep.Body, &out))
		reasoning, _ := out["reasoning"].(map[string]any)
		require.NotNil(t, reasoning, "budget %d", tc.budget)
		assert.Equal(t, tc.want, reasoning["effort"], "budget %d", tc.budget)
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
	// The preceding reasoning item's signature is also carried on the tool_use id
	// (the Claude Code round-trip drops the thinking block but preserves the id),
	// so the id is the call_id plus an opaque reasoning-signature suffix.
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
}

// gemini-3.x (native) must receive a thinkingConfig derived from the Anthropic
// thinking budget so it reasons. Gemini 3.x uses the string `thinkingLevel`;
// the legacy numeric `thinkingBudget` is suboptimal for 3.x and mixing both 400s.
func TestPrepareGemini_ThinkingBudgetToThinkingConfig(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(nil, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview", Capabilities: router.Lookup("gemini-3.1-pro-preview")})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	gen, ok := out["generationConfig"].(map[string]any)
	require.True(t, ok, "generationConfig present")
	tc, ok := gen["thinkingConfig"].(map[string]any)
	require.True(t, ok, "thinkingConfig set from thinking budget")
	assert.Equal(t, "high", tc["thinkingLevel"], "high budget -> gemini-3.x thinkingLevel high")
	_, hasBudget := tc["thinkingBudget"]
	assert.False(t, hasBudget, "gemini-3.x must NOT send the legacy thinkingBudget")
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
	assert.EqualValues(t, 24576, tc["thinkingBudget"], "high budget -> gemini-2.5 thinkingBudget 24576")
	_, hasLevel := tc["thinkingLevel"]
	assert.False(t, hasLevel, "gemini-2.5 must NOT send thinkingLevel")
}
