package translate_test

import (
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
	// Load-bearing test: thought_signature on a prior assistant tool_call must
	// land as thoughtSignature on the Gemini functionCall part. This is the
	// whole reason the native adapter exists.
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
		`"tool_choice":"auto"`:                                        "AUTO",
		`"tool_choice":"none"`:                                        "NONE",
		`"tool_choice":"required"`:                                    "ANY",
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

func TestPrepareGemini_ReasoningEffortMapsToThinkingBudget(t *testing.T) {
	cases := map[string]int{"low": 1024, "medium": 8192, "high": 24576, "none": 0}
	for effort, budget := range cases {
		body := []byte(`{"messages":[{"role":"user","content":"x"}],"reasoning_effort":"` + effort + `"}`)
		env, _ := translate.ParseOpenAI(body)
		prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
		require.NoError(t, err, effort)
		out := mustUnmarshal(t, prep.Body)
		gc := out["generationConfig"].(map[string]any)
		tc := gc["thinkingConfig"].(map[string]any)
		assert.EqualValues(t, budget, tc["thinkingBudget"], effort)
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
				 "thought_signature":"ANTHROPIC_SIG"}
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
	assert.Equal(t, "ANTHROPIC_SIG", p["thoughtSignature"])
	fc := p["functionCall"].(map[string]any)
	assert.Equal(t, "bash", fc["name"])

	tr := contents[2].(map[string]any)
	frPart := tr["parts"].([]any)[0].(map[string]any)
	fr := frPart["functionResponse"].(map[string]any)
	assert.Equal(t, "bash", fr["name"])
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
	// Load-bearing: response-side signature surfaces on tool_call.function.
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
	assert.Equal(t, "GEMINI_SIG", fn["thought_signature"])
	assert.Equal(t, "GEMINI_SIG", tc["thought_signature"])
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

func TestGeminiToAnthropicResponse_ToolUsePreservesThoughtSignature(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"bash","args":{"command":"ls"}},
			 "thoughtSignature":"GS"}
		]},"finishReason":"STOP"}]
	}`)
	out, err := translate.GeminiToAnthropicResponse(body, "gemini-x")
	require.NoError(t, err)
	resp := mustUnmarshal(t, out)
	assert.Equal(t, "tool_use", resp["stop_reason"])
	blocks := resp["content"].([]any)
	tu := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", tu["type"])
	assert.Equal(t, "GS", tu["thought_signature"])
	assert.Equal(t, "bash", tu["name"])
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
	assert.Contains(t, body, `"thought_signature":"SIG"`)
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
