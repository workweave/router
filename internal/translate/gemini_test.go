package translate_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- OpenAI → Gemini request -----

func TestPrepareGemini_FromOpenAI_SimpleText(t *testing.T) {
	body := []byte(`{
		"model": "gemini-3.1-flash-lite-preview",
		"messages": [
			{"role": "system", "content": "You are helpful."},
			{"role": "user", "content": "list /tmp"}
		]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-flash-lite-preview"})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	sys := must(t, out, "systemInstruction")
	assert.Equal(t, "You are helpful.", sys.(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])

	contents := must(t, out, "contents").([]any)
	require.Len(t, contents, 1)
	first := contents[0].(map[string]any)
	assert.Equal(t, "user", first["role"])
	parts := first["parts"].([]any)
	assert.Equal(t, "list /tmp", parts[0].(map[string]any)["text"])
}

func TestPrepareGemini_MultipleSystemMessagesConcatenated(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "rule one"},
			{"role": "system", "content": "rule two"},
			{"role": "user", "content": "go"}
		]
	}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	sys := out["systemInstruction"].(map[string]any)
	text := sys["parts"].([]any)[0].(map[string]any)["text"].(string)
	assert.Equal(t, "rule one\nrule two", text)
}

func TestPrepareGemini_AssistantToolCallsRoundTripsThoughtSignature(t *testing.T) {
	// Load-bearing: thought_signature on a prior assistant tool_call must land
	// as thoughtSignature on the Gemini functionCall part.
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "list /tmp"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "c1", "type": "function",
				 "function": {"name": "bash", "arguments": "{\"command\":\"ls /tmp\"}",
				              "thought_signature": "OPAQUE_SIG_BYTES"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "f1\nf2"}
		]
	}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	contents := out["contents"].([]any)
	require.Len(t, contents, 3)

	// model turn carries functionCall + thoughtSignature.
	model := contents[1].(map[string]any)
	assert.Equal(t, "model", model["role"])
	parts := model["parts"].([]any)
	require.Len(t, parts, 1)
	p := parts[0].(map[string]any)
	fc := p["functionCall"].(map[string]any)
	assert.Equal(t, "bash", fc["name"])
	args := fc["args"].(map[string]any)
	assert.Equal(t, "ls /tmp", args["command"])
	assert.Equal(t, "OPAQUE_SIG_BYTES", p["thoughtSignature"])

	// tool turn must surface as user-role functionResponse with the same name.
	toolTurn := contents[2].(map[string]any)
	assert.Equal(t, "user", toolTurn["role"])
	tps := toolTurn["parts"].([]any)
	fr := tps[0].(map[string]any)["functionResponse"].(map[string]any)
	assert.Equal(t, "bash", fr["name"])
	assert.Equal(t, "f1\nf2", fr["response"].(map[string]any)["result"])
}

func TestPrepareGemini_ToolsMappedToFunctionDeclarations(t *testing.T) {
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [
			{"type":"function","function":{
				"name":"bash","description":"Run bash",
				"parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}
			}}
		]
	}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	tools := out["tools"].([]any)
	require.Len(t, tools, 1)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	require.Len(t, decls, 1)
	d := decls[0].(map[string]any)
	assert.Equal(t, "bash", d["name"])
	assert.Equal(t, "Run bash", d["description"])
	assert.NotNil(t, d["parameters"])
}

func TestPrepareGemini_ToolChoiceVariants(t *testing.T) {
	cases := map[string]string{
		`"tool_choice":"auto"`:     "AUTO",
		`"tool_choice":"none"`:     "NONE",
		`"tool_choice":"required"`: "ANY",
		`"tool_choice":{"type":"function","function":{"name":"bash"}}`: "ANY",
	}
	for tc, mode := range cases {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],` + tc + `}`)
		env, _ := translate.ParseOpenAI(body)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
		require.NoError(t, err, tc)
		out := mustUnmarshal(t, prep.Body)
		got := out["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]
		assert.Equal(t, mode, got, tc)
	}
}

func TestPrepareGemini_FromAnthropic_ToolChoiceVariants(t *testing.T) {
	cases := map[string]string{
		`"tool_choice":{"type":"auto"}`:               "AUTO",
		`"tool_choice":{"type":"none"}`:               "NONE",
		`"tool_choice":{"type":"any"}`:                "ANY",
		`"tool_choice":{"type":"tool","name":"bash"}`: "ANY",
	}
	for tc, mode := range cases {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],` + tc + `}`)
		env, _ := translate.ParseAnthropic(body)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
		require.NoError(t, err, tc)
		out := mustUnmarshal(t, prep.Body)
		got := out["toolConfig"].(map[string]any)["functionCallingConfig"].(map[string]any)["mode"]
		assert.Equal(t, mode, got, tc)
	}
}

// TestPrepareGemini_UnrecognizedToolChoiceDoesNotEnableValidatedMode guards a
// regression from the tool_choice normalization: a present-but-malformed
// tool_choice (unknown string, or an Anthropic object with an unknown type)
// must NOT be treated the same as a truly absent tool_choice. On Gemini 3.x,
// absent/auto upgrades to mode=VALIDATED; a malformed value must fall back to
// legacy no-toolConfig behavior instead of silently getting that upgrade.
func TestPrepareGemini_UnrecognizedToolChoiceDoesNotEnableValidatedMode(t *testing.T) {
	tools := `"tools":[{"name":"bash","description":"b","input_schema":{"type":"object"}}]`

	t.Run("openai_source_unknown_string", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],` + tools + `,"tool_choice":"bogus"}`)
		env, err := translate.ParseOpenAI(body)
		require.NoError(t, err)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
		require.NoError(t, err)
		out := mustUnmarshal(t, prep.Body)
		_, hasToolConfig := out["toolConfig"]
		assert.False(t, hasToolConfig, "malformed tool_choice must not trigger VALIDATED mode")
	})

	t.Run("anthropic_source_unknown_type", func(t *testing.T) {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],` + tools + `,"tool_choice":{"type":"bogus"}}`)
		env, err := translate.ParseAnthropic(body)
		require.NoError(t, err)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
		require.NoError(t, err)
		out := mustUnmarshal(t, prep.Body)
		_, hasToolConfig := out["toolConfig"]
		assert.False(t, hasToolConfig, "malformed tool_choice must not trigger VALIDATED mode")
	})
}

func TestPrepareGemini_ReasoningEffortMapsToThinkingBudget(t *testing.T) {
	// No target model => legacy (gemini-2.5) path: numeric thinkingBudget.
	cases := map[string]int{"low": 1024, "medium": 8192, "high": 24576, "none": 0}
	for effort, budget := range cases {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"` + effort + `"}`)
		env, _ := translate.ParseOpenAI(body)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
		require.NoError(t, err, effort)
		out := mustUnmarshal(t, prep.Body)
		gc := out["generationConfig"].(map[string]any)
		tc := gc["thinkingConfig"].(map[string]any)
		assert.EqualValues(t, budget, tc["thinkingBudget"], effort)
	}
}

func TestPrepareGemini_ReasoningEffortMapsToThinkingLevel_Gemini3x(t *testing.T) {
	// gemini-3.x uses string thinkingLevel, not thinkingBudget. "none" is
	// omitted — 3.x can't disable thinking.
	for _, effort := range []string{"low", "medium", "high"} {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"` + effort + `"}`)
		env, _ := translate.ParseOpenAI(body)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
		require.NoError(t, err, effort)
		out := mustUnmarshal(t, prep.Body)
		gc := out["generationConfig"].(map[string]any)
		tc := gc["thinkingConfig"].(map[string]any)
		assert.Equal(t, effort, tc["thinkingLevel"], effort)
		_, hasBudget := tc["thinkingBudget"]
		assert.False(t, hasBudget, "gemini-3.x must not send thinkingBudget (%s)", effort)
	}
}

func TestPrepareGemini_StreamHintHeaderSet(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"}],"stream":true}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader))
}

func TestPrepareGemini_NoStreamHintWhenNonStreaming(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"x"}]}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)
	assert.Empty(t, prep.Headers.Get(translate.GeminiStreamHintHeader))
}

// ----- Anthropic → Gemini request -----

func TestPrepareGemini_FromAnthropic_SystemAndToolUseRoundTripsSignature(t *testing.T) {
	body := []byte(`{
		"system": "be helpful",
		"messages": [
			{"role":"user","content":[{"type":"text","text":"list"}]},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"toolu_x","name":"bash",
				 "input":{"command":"ls"},
				 "thought_signature":"ANTHROPIC_SIG_00"}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_x","content":"f1"}
			]}
		],
		"tools": [{"name":"bash","description":"b","input_schema":{"type":"object"}}]
	}`)
	env, _ := translate.ParseAnthropic(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{Capabilities: router.ModelSpec{}})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	assert.NotNil(t, out["systemInstruction"])

	contents := out["contents"].([]any)
	require.Len(t, contents, 3)

	model := contents[1].(map[string]any)
	parts := model["parts"].([]any)
	p := parts[0].(map[string]any)
	assert.Equal(t, "ANTHROPIC_SIG_00", p["thoughtSignature"])
	fc := p["functionCall"].(map[string]any)
	assert.Equal(t, "bash", fc["name"])

	tr := contents[2].(map[string]any)
	frPart := tr["parts"].([]any)[0].(map[string]any)
	fr := frPart["functionResponse"].(map[string]any)
	assert.Equal(t, "bash", fr["name"])
}

func TestPrepareGemini_FromAnthropic_DropsOrphanedToolResult(t *testing.T) {
	// tool_result without matching tool_use — name lookup yields "".
	// The emitter must skip it rather than producing empty function_response.name.
	body := []byte(`{
		"messages": [
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"orphan","content":"stale result"},
				{"type":"text","text":"hello"}
			]}
		]
	}`)
	env, _ := translate.ParseAnthropic(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{Capabilities: router.ModelSpec{}})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	contents := out["contents"].([]any)
	require.Len(t, contents, 1)
	parts := contents[0].(map[string]any)["parts"].([]any)
	require.Len(t, parts, 1, "orphaned tool_result must be dropped")
	assert.NotNil(t, parts[0].(map[string]any)["text"], "only the text part should remain")
}

func TestPrepareGemini_FromOpenAI_DropsOrphanedToolMessage(t *testing.T) {
	body := []byte(`{
		"model": "gemini-3.1-flash-lite-preview",
		"messages": [
			{"role":"user","content":"hi"},
			{"role":"tool","tool_call_id":"orphan","content":"stale result"},
			{"role":"assistant","content":"done"},
			{"role":"user","content":"next"}
		]
	}`)
	env, _ := translate.ParseOpenAI(body)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{Capabilities: router.ModelSpec{}})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	contents := out["contents"].([]any)
	// user "hi", model "done", user "next" — orphaned tool message skipped.
	require.Len(t, contents, 3)
	for _, c := range contents {
		entry := c.(map[string]any)
		for _, p := range entry["parts"].([]any) {
			part := p.(map[string]any)
			_, hasFR := part["functionResponse"]
			assert.False(t, hasFR, "orphaned functionResponse must not appear")
		}
	}
}

// ----- Gemini → OpenAI response -----

func TestGeminiToOpenAIResponse_TextOnly(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}
	}`)
	out, err := translate.GeminiToOpenAIResponse(body, "gemini-3.1-flash-lite-preview")
	require.NoError(t, err)
	resp := mustUnmarshal(t, out)
	assert.Equal(t, "chat.completion", resp["object"])
	assert.Equal(t, "gemini-3.1-flash-lite-preview", resp["model"])
	choice := resp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	assert.Equal(t, "hi", msg["content"])
	assert.Equal(t, "stop", choice["finish_reason"])
	usage := resp["usage"].(map[string]any)
	assert.EqualValues(t, 3, usage["prompt_tokens"])
	assert.EqualValues(t, 1, usage["completion_tokens"])
	assert.EqualValues(t, 4, usage["total_tokens"])
	id := resp["id"].(string)
	assert.True(t, strings.HasPrefix(id, "chatcmpl-"))
	assert.Len(t, id, len("chatcmpl-")+16)
}

func TestGeminiToOpenAIResponse_ToolCallPreservesThoughtSignature(t *testing.T) {
	// Signature is smuggled into the tool-call id, not emitted as an off-spec field.
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"bash","args":{"command":"ls /tmp"}},
			 "thoughtSignature":"GEMINI_SIG"}
		]},"finishReason":"STOP","index":0}]
	}`)
	out, err := translate.GeminiToOpenAIResponse(body, "gemini-3.1-flash-lite-preview")
	require.NoError(t, err)
	resp := mustUnmarshal(t, out)
	choice := resp["choices"].([]any)[0].(map[string]any)
	assert.Equal(t, "tool_calls", choice["finish_reason"])
	tc := choice["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	fn := tc["function"].(map[string]any)
	assert.Equal(t, "bash", fn["name"])
	assert.Equal(t, `{"command":"ls /tmp"}`, fn["arguments"])
	assert.NotContains(t, fn, "thought_signature", "off-spec field must not be emitted")
	assert.NotContains(t, tc, "thought_signature", "off-spec field must not be emitted")
	encoded := base64.RawURLEncoding.EncodeToString([]byte("GEMINI_SIG"))
	assert.Contains(t, tc["id"].(string), encoded, "signature rides in the tool-call id")
}

func TestGeminiToOpenAIResponse_FinishReasonMapping(t *testing.T) {
	cases := map[string]string{
		"STOP":       "stop",
		"MAX_TOKENS": "length",
		"SAFETY":     "content_filter",
		"RECITATION": "content_filter",
		"":           "stop",
	}
	for fr, want := range cases {
		body := []byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"` + fr + `"}]}`)
		out, _ := translate.GeminiToOpenAIResponse(body, "m")
		resp := mustUnmarshal(t, out)
		got := resp["choices"].([]any)[0].(map[string]any)["finish_reason"]
		assert.Equal(t, want, got, fr)
	}
}

// ----- Gemini → Anthropic response -----

// Load-bearing regression: Gemini 3.x multi-turn tool use when the client
// drops the off-spec thought_signature field on deserialization — the
// signature must still be smuggled back via the id channel.
func TestPrepareGemini_FromAnthropic_ToolUseSignatureSurvivesUnknownFieldStripping(t *testing.T) {
	// Turn 1: construct the tool_use block by hand, replicating what
	// embedSignatureInID produces (id + "__thought__" + base64(sig)).
	smuggledID := "toolu_test__thought__" + base64.RawURLEncoding.EncodeToString([]byte("OPAQUE_GEMINI_SIG000"))
	tu := map[string]any{
		"type":  "tool_use",
		"id":    smuggledID,
		"name":  "bash",
		"input": map[string]any{"command": "ls"},
	}

	// Turn 3: client sends history back. Router translates Anthropic → Gemini.
	req := map[string]any{
		"model": "claude-opus-4-7",
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{"role": "assistant", "content": []any{tu}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": smuggledID, "content": "f1\nf2"},
			}},
		},
	}
	reqBody, err := json.Marshal(req)
	require.NoError(t, err)
	env, err := translate.ParseAnthropic(reqBody)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	contents := out["contents"].([]any)
	require.Len(t, contents, 3)
	model := contents[1].(map[string]any)
	parts := model["parts"].([]any)
	p := parts[0].(map[string]any)
	// The signature is what Gemini 3.x rejects requests for when missing.
	assert.Equal(t, "OPAQUE_GEMINI_SIG000", p["thoughtSignature"])
	fc := p["functionCall"].(map[string]any)
	assert.Equal(t, "bash", fc["name"])
	// And the functionResponse must still resolve the name from the smuggled id.
	tr := contents[2].(map[string]any)
	frPart := tr["parts"].([]any)[0].(map[string]any)
	fr := frPart["functionResponse"].(map[string]any)
	assert.Equal(t, "bash", fr["name"])
}

// Same as above but via OpenAI Chat Completions wire format; tool_call.id is
// the round-trip channel and is preserved by every Chat Completions client.
func TestPrepareGemini_FromOpenAI_ToolCallSignatureSurvivesUnknownFieldStripping(t *testing.T) {
	geminiResp := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"bash","args":{"command":"ls"}},
			 "thoughtSignature":"OPAQUE_SIG_2"}
		]},"finishReason":"STOP"}]
	}`)
	openaiOut, err := translate.GeminiToOpenAIResponse(geminiResp, "gemini-3.1-pro-preview")
	require.NoError(t, err)
	resp := mustUnmarshal(t, openaiOut)
	choice := resp["choices"].([]any)[0].(map[string]any)
	tc := choice["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	smuggledID := tc["id"].(string)
	require.Contains(t, smuggledID, "__thought__")

	// Strip the explicit signature fields on both the tool_call and the
	// nested function — simulating a client that only preserves spec fields.
	delete(tc, "thought_signature")
	fn := tc["function"].(map[string]any)
	delete(fn, "thought_signature")

	req := map[string]any{
		"model": "gpt-x",
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{tc}},
			map[string]any{"role": "tool", "tool_call_id": smuggledID, "content": "f1\nf2"},
		},
	}
	reqBody, err := json.Marshal(req)
	require.NoError(t, err)
	env, err := translate.ParseOpenAI(reqBody)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)
	out := mustUnmarshal(t, prep.Body)
	contents := out["contents"].([]any)
	require.GreaterOrEqual(t, len(contents), 2)
	model := contents[1].(map[string]any)
	parts := model["parts"].([]any)
	p := parts[0].(map[string]any)
	assert.Equal(t, "OPAQUE_SIG_2", p["thoughtSignature"])
}

// ----- Streaming -----

func TestGeminiStream_TextDeltas(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-x", nil)
	tr.Header().Set("Content-Type", "text/event-stream")
	tr.WriteHeader(http.StatusOK)

	chunks := []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"hi "}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[{"text":"world"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}` + "\n\n",
	}
	for _, c := range chunks {
		_, err := tr.Write([]byte(c))
		require.NoError(t, err)
	}
	require.NoError(t, tr.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"role":"assistant"`)
	assert.Contains(t, body, `"content":"hi "`)
	assert.Contains(t, body, `"content":"world"`)
	assert.Contains(t, body, `"finish_reason":"stop"`)
	assert.Contains(t, body, `"prompt_tokens":3`)
	assert.Contains(t, body, "data: [DONE]")
}

func TestGeminiStream_FunctionCallChunkPreservesThoughtSignature(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-x", nil)
	tr.Header().Set("Content-Type", "text/event-stream")
	tr.WriteHeader(http.StatusOK)

	chunk := `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"bash","args":{"command":"ls"}},"thoughtSignature":"SIG"}]},"finishReason":"STOP"}]}` + "\n\n"
	_, err := tr.Write([]byte(chunk))
	require.NoError(t, err)
	require.NoError(t, tr.Finalize())

	body := rec.Body.String()
	assert.Contains(t, body, `"tool_calls"`)
	assert.Contains(t, body, `"name":"bash"`)
	assert.NotContains(t, body, `"thought_signature"`, "off-spec field must not be emitted")
	encoded := base64.RawURLEncoding.EncodeToString([]byte("SIG"))
	assert.Contains(t, body, encoded, "signature rides in the tool-call id")
	assert.Contains(t, body, `"finish_reason":"tool_calls"`)
	assert.Contains(t, body, "data: [DONE]")
}

func TestGeminiStream_NonStreamingErrorTranslated(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-x", nil)
	tr.Header().Set("Content-Type", "application/json")
	tr.WriteHeader(http.StatusBadRequest)
	_, err := tr.Write([]byte(`{"error":{"code":400,"message":"bad","status":"INVALID_ARGUMENT"}}`))
	require.NoError(t, err)
	require.NoError(t, tr.Finalize())

	resp := mustUnmarshal(t, rec.Body.Bytes())
	e := resp["error"].(map[string]any)
	assert.Equal(t, "bad", e["message"])
	assert.Equal(t, "invalid_argument", e["type"])
}

// ----- helpers -----

func mustUnmarshal(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func must(t *testing.T, m map[string]any, k string) any {
	t.Helper()
	v, ok := m[k]
	require.True(t, ok, "missing key %s", k)
	return v
}

func TestPrepareGemini_StripsJSONSchemaFieldsGoogleRejects(t *testing.T) {
	// Regression (#62, re-regressed by #764's switch to an allowlist and
	// fixed again here): Claude Code tool defs — including the Agent/Task
	// subagent tool itself — include JSON Schema fields (`$schema`,
	// `additionalProperties`, `propertyNames`) Google's function-calling API
	// rejects outright. These must be silently dropped, not turned into a
	// hard tool-declaration failure: #764 briefly did the latter, which
	// 502'd every real Claude Code turn against any Gemini 3.x model that
	// included the Agent tool (caught 2026-07-21 onboarding gemini-3.6-flash
	// / gemini-3.5-flash-lite, but affected already-deployed Gemini models
	// too).
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"WebFetch",
			"description":"Fetch a URL",
			"input_schema":{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"type":"object",
				"additionalProperties":false,
				"properties":{
					"url":{"type":"string","description":"URL to fetch"},
					"params":{
						"type":"object",
						"additionalProperties":{"type":"string"},
						"propertyNames":{"pattern":"^[A-Za-z]+$"}
					}
				},
				"required":["url"]
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	tools := out["tools"].([]any)
	require.Len(t, tools, 1)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	require.Len(t, decls, 1)
	params := decls[0].(map[string]any)["parameters"].(map[string]any)

	// The rejected keys are gone at every level...
	assert.NotContains(t, params, "$schema")
	assert.NotContains(t, params, "additionalProperties")
	nested := params["properties"].(map[string]any)["params"].(map[string]any)
	assert.NotContains(t, nested, "additionalProperties")
	assert.NotContains(t, nested, "propertyNames")

	// ...but the schema's actual meaning survives.
	assert.Equal(t, "object", params["type"])
	assert.Equal(t, []any{"url"}, params["required"])
	props := params["properties"].(map[string]any)
	assert.Equal(t, "string", props["url"].(map[string]any)["type"])
}

func TestPrepareGemini_WidensExclusiveBoundsToInclusive(t *testing.T) {
	// Regression: exclusiveMinimum/exclusiveMaximum are unsupported by
	// Gemini's function-calling schema; widen to inclusive bounds.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"Read",
			"input_schema":{
				"type":"object",
				"properties":{
					"limit":{"type":"integer","exclusiveMinimum":0},
					"offset":{"type":"integer","exclusiveMaximum":100,"minimum":0},
					"page":{"type":"integer","exclusiveMinimum":0,"minimum":5},
					"strict":{"type":"integer","exclusiveMinimum":5,"minimum":0}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	decls := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	props := decls[0].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)

	limit := props["limit"].(map[string]any)
	assert.NotContains(t, limit, "exclusiveMinimum")
	assert.Equal(t, float64(0), limit["minimum"], "exclusiveMinimum widens to the same-valued inclusive minimum")

	// exclusiveMaximum widens to maximum with the same value when there's no
	// sibling maximum to preserve; the untouched sibling minimum survives.
	offset := props["offset"].(map[string]any)
	assert.NotContains(t, offset, "exclusiveMaximum")
	assert.Equal(t, float64(0), offset["minimum"])
	assert.Equal(t, float64(100), offset["maximum"])

	// When the explicit sibling is the tighter bound, it wins over the
	// widened (weaker) exclusive value.
	page := props["page"].(map[string]any)
	assert.NotContains(t, page, "exclusiveMinimum")
	assert.Equal(t, float64(5), page["minimum"], "explicit minimum (5) is tighter than widened exclusiveMinimum (0)")

	// When the exclusive bound is the tighter one, the widened value wins
	// over the weaker explicit sibling instead of being discarded.
	strict := props["strict"].(map[string]any)
	assert.NotContains(t, strict, "exclusiveMinimum")
	assert.Equal(t, float64(5), strict["minimum"], "widened exclusiveMinimum (5) is tighter than explicit minimum (0)")
}

func TestPrepareGemini_PrunesDanglingRequired(t *testing.T) {
	// Regression: Gemini 400s when "required" names a property not in
	// "properties" — valid JSON Schema, so MCP tool schemas can carry it; prune it.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"DoThing",
			"description":"d",
			"input_schema":{
				"type":"object",
				"properties":{"x":{"type":"string"}},
				"required":["x","ghost"]
			}
		},{
			"name":"AllDangling",
			"description":"d",
			"input_schema":{
				"type":"object",
				"properties":{"a":{"type":"string"}},
				"required":["missing"]
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	_, err = env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.ErrorIs(t, err, translate.ErrGeminiSchemaIncompatible)
}

func TestPrepareGemini_PreservesPropertyNamedRequired(t *testing.T) {
	// A parameter literally named "required" is a property schema, not the
	// "required" keyword, and must survive pruning untouched.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"DoThing",
			"description":"d",
			"input_schema":{
				"type":"object",
				"properties":{
					"required":{
						"type":"object",
						"properties":{"inner":{"type":"string"}},
						"required":["inner"]
					}
				},
				"required":["required"]
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)

	// The schema-level "required" naming the real "required" property is kept.
	assert.Equal(t, []any{"required"}, params["required"])
	// The property named "required" is preserved as an object schema, not
	// rewritten into an array or deleted.
	prop, ok := params["properties"].(map[string]any)["required"].(map[string]any)
	require.True(t, ok, `parameter named "required" must remain a schema object`)
	assert.Equal(t, "object", prop["type"])
	// Its own nested "required" keyword (valid: "inner" exists) is intact.
	assert.Equal(t, []any{"inner"}, prop["required"])
}

func TestPrepareGemini_StripsVendorExtensionAndDollarPrefixedKeys(t *testing.T) {
	// Regression: MCP schemas derived from Google APIs embed vendor extensions
	// (`x-google-*`) at every level; Gemini 400s on unknown fields. Strip any
	// "x-" or "$" prefixed key instead of maintaining an allowlist.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"sheets_get",
			"input_schema":{
				"$schema":"http://json-schema.org/draft-07/schema#",
				"$id":"urn:weave:test",
				"type":"object",
				"x-google-resource":"sheet",
				"properties":{
					"range":{
						"type":"string",
						"x-google-enum-descriptions":["A","B"],
						"enum":["A1","B2"]
					},
					"nested":{
						"type":"object",
						"x-google-foo":"bar",
						"properties":{
							"leaf":{"type":"string","x-vendor-thing":"y"}
						}
					}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	_, err = env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.ErrorIs(t, err, translate.ErrGeminiSchemaIncompatible)
}

func TestSanitizeSchemaForGemini_PreservesSupportedFields(t *testing.T) {
	// Defense-in-depth: exhaustively confirms which keys survive sanitization.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"Edit",
			"input_schema":{
				"type":"object",
				"description":"Edit a file",
				"properties":{
					"path":{"type":"string","format":"date-time"},
					"mode":{"type":"string","enum":["replace","append"]},
					"line":{"type":"integer","nullable":true}
				},
				"required":["path","mode"]
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)

	assert.Equal(t, "object", params["type"])
	assert.Equal(t, "Edit a file", params["description"])
	props := params["properties"].(map[string]any)
	assert.Equal(t, "date-time", props["path"].(map[string]any)["format"], "supported format must survive")
	assert.Equal(t, []any{"replace", "append"}, props["mode"].(map[string]any)["enum"], "enum must survive")
	assert.Equal(t, true, props["line"].(map[string]any)["nullable"], "nullable must survive")
	assert.Equal(t, []any{"path", "mode"}, params["required"], "required must survive")
}

func TestPrepareGemini_DropsUnsupportedFormatValues(t *testing.T) {
	// Regression: Gemini's Schema only accepts a narrow "format" set (strings:
	// enum, date-time; numbers: float/double/int32/int64). MCP schemas routinely
	// carry "uri"/"email"/"uuid" etc., which Gemini 400s on; drop unsupported
	// values but keep the rest of the property schema intact.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"submit",
			"input_schema":{
				"type":"object",
				"properties":{
					"homepage":{"type":"string","format":"uri","description":"site"},
					"contact":{"type":"string","format":"email"},
					"when":{"type":"string","format":"date-time"},
					"score":{"type":"number","format":"double"}
				},
				"required":["homepage"]
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	decls := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	props := decls[0].(map[string]any)["parameters"].(map[string]any)["properties"].(map[string]any)

	homepage := props["homepage"].(map[string]any)
	assert.Equal(t, "string", homepage["type"])
	assert.Equal(t, "site", homepage["description"])
	assert.NotContains(t, homepage, "format", "unsupported format value 'uri' must be dropped silently")

	contact := props["contact"].(map[string]any)
	assert.Equal(t, "string", contact["type"])
	assert.NotContains(t, contact, "format", "unsupported format value 'email' must be dropped silently")

	// Supported formats survive untouched.
	when := props["when"].(map[string]any)
	assert.Equal(t, "date-time", when["format"])
	score := props["score"].(map[string]any)
	assert.Equal(t, "double", score["format"])
}

func TestPrepareGemini_CollapsesItemsBool(t *testing.T) {
	// Regression: MCP schemas (e.g. SigNoz) use `"items": true` (JSON Schema:
	// any items allowed); Gemini's proto Schema.items rejects boolean values.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"create_dashboard",
			"input_schema":{
				"type":"object",
				"properties":{
					"args":{
						"type":"array",
						"items":true
					},
					"disabled":{
						"type":"array",
						"items":false
					},
					"links":{
						"type":"array",
						"items":{"type":"string"}
					}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	_, err = env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.ErrorIs(t, err, translate.ErrGeminiSchemaIncompatible)
}

func TestPrepareGemini_CollapsesArrayTypeField(t *testing.T) {
	// Regression: MCP schemas (e.g. Pylon) use array-typed "type" like
	// ["array","null"]; Gemini expects a single type plus a "nullable" bool.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"create_issue",
			"input_schema":{
				"type":"object",
				"properties":{
					"tags":{
						"type":["array","null"],
						"description":"optional tags",
						"items":{"type":"string"}
					},
					"title":{"type":"string"},
					"score":{"type":["number","null"]}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	tags := props["tags"].(map[string]any)
	assert.Equal(t, "array", tags["type"], "array-type type must collapse to primary non-null type")
	assert.Equal(t, true, tags["nullable"], "nullable must be set when null appears in type array")
	assert.NotNil(t, tags["items"], "items must survive collapseTypeArray")

	assert.Equal(t, "string", props["title"].(map[string]any)["type"], "single-string type must be unchanged")

	score := props["score"].(map[string]any)
	assert.Equal(t, "number", score["type"], "number type must survive")
	assert.Equal(t, true, score["nullable"], "nullable must be set for [number, null]")
}

func TestPrepareGemini_PreservesArrayMissingItems(t *testing.T) {
	// A missing items constraint accepts arbitrary item values. Inventing an
	// item schema would narrow the client contract, so it must remain absent.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"odd-tool",
			"input_schema":{
				"type":"object",
				"properties":{
					"tools":{"type":"array","description":"a list of tools"},
					"nested":{
						"type":"object",
						"properties":{
							"items":{"type":"array"}
						}
					}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	toolsField := props["tools"].(map[string]any)
	assert.Equal(t, "array", toolsField["type"])
	assert.NotContains(t, toolsField, "items")

	nestedItems := props["nested"].(map[string]any)["properties"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, "array", nestedItems["type"])
	assert.NotContains(t, nestedItems, "items")
}

func TestPrepareGemini_PreservesEnumValueTypes(t *testing.T) {
	// Empty-string enum members are meaningful accepted values. Sanitization
	// must preserve them rather than silently broadening the schema.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"odd-tool",
			"input_schema":{
				"type":"object",
				"properties":{
					"operator":{"type":"string","enum":["","eq","neq","gt"]},
					"all_empty":{"type":"string","enum":["",""]},
					"normal":{"type":"string","enum":["a","b"]}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	operator := props["operator"].(map[string]any)
	assert.Equal(t, []any{"", "eq", "neq", "gt"}, operator["enum"])

	allEmpty := props["all_empty"].(map[string]any)
	assert.Equal(t, []any{"", ""}, allEmpty["enum"])

	normal := props["normal"].(map[string]any)
	assert.Equal(t, []any{"a", "b"}, normal["enum"], "well-formed enums must pass through unchanged")
}

func TestPrepareGemini_UserDefinedPropertyNamedProperties(t *testing.T) {
	// A user-defined property named "properties" must not be mistaken for the
	// JSON Schema "properties" keyword. Its value schema must still be
	// filtered — and additionalProperties within it silently dropped, not
	// treated as a rejection (see TestPrepareGemini_StripsJSONSchemaFieldsGoogleRejects).
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"track_event",
			"input_schema":{
				"type":"object",
				"properties":{
					"eventName":{"type":"string"},
					"properties":{
						"type":"object",
						"additionalProperties":true,
						"description":"Additional properties"
					}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	decls := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	params := decls[0].(map[string]any)["parameters"].(map[string]any)
	properties := params["properties"].(map[string]any)

	nested, ok := properties["properties"].(map[string]any)
	require.True(t, ok, `the user-defined "properties" key must survive as an object schema`)
	assert.Equal(t, "object", nested["type"])
	assert.Equal(t, "Additional properties", nested["description"])
	assert.NotContains(t, nested, "additionalProperties")
}

func TestPrepareGemini_GeminiFormatSanitizesTools(t *testing.T) {
	// The same-format (FormatGemini) path must also sanitize tool schemas —
	// additionalProperties dropped silently, not rejected (see
	// TestPrepareGemini_StripsJSONSchemaFieldsGoogleRejects).
	body := []byte(`{
		"model": "gemini-3.1-pro-preview",
		"stream": false,
		"contents": [{"role":"user","parts":[{"text":"hi"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name":"test",
				"parameters":{
					"type":"object",
					"additionalProperties":false,
					"properties":{"x":{"type":"string"}}
				}
			}]
		}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	decls := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	params := decls[0].(map[string]any)["parameters"].(map[string]any)
	assert.NotContains(t, params, "additionalProperties")
	assert.Equal(t, "object", params["type"])
}

func TestPrepareGemini_GeminiFormatInlinesSchemaRefs(t *testing.T) {
	// Same-format Gemini path must inline $ref/$defs before sanitization,
	// otherwise Gemini's allowlist strips them silently.
	body := []byte(`{
		"model": "gemini-3.1-pro-preview",
		"contents": [{"role":"user","parts":[{"text":"hi"}]}],
		"tools": [{
			"functionDeclarations": [{
				"name":"test",
				"parameters":{
					"type":"object",
					"properties":{
						"item": {"$ref": "#/$defs/Item"}
					},
					"$defs": {
						"Item": {"type":"object","properties":{"name":{"type":"string"}}}
					}
				}
			}]
		}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	item := params["properties"].(map[string]any)["item"].(map[string]any)
	assert.NotContains(t, item, "$ref", "$ref must be inlined, not left as pointer")
	assert.Equal(t, "object", item["type"], "inlined item should have type:object")
	assert.NotContains(t, params, "$defs", "$defs must be removed after inlining")
}

func TestPrepareGemini_ConvertsBoolPropertySchemas(t *testing.T) {
	// JSON Schema allows boolean schemas as property values (true = any type).
	// Gemini's proto Schema rejects them. They must be converted to empty Schema.
	body := []byte(`{
		"messages": [{"role":"user","content":"hi"}],
		"tools": [{
			"name":"create_dashboard",
			"input_schema":{
				"type":"object",
				"properties":{
					"queryData":true,
					"softMax":true,
					"realProp":{"type":"string"}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	// Boolean true → empty Schema {}
	qd, ok := props["queryData"].(map[string]any)
	require.True(t, ok, "queryData=true should become empty schema object, got %T", props["queryData"])
	assert.NotNil(t, qd)

	sm, ok := props["softMax"].(map[string]any)
	require.True(t, ok, "softMax=true should become empty schema object, got %T", props["softMax"])
	assert.NotNil(t, sm)

	// Real property must survive
	rp, ok := props["realProp"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", rp["type"])
}
