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
	// The id must also carry the signature so typed-SDK clients (Claude Code)
	// that drop unknown content-block fields still round-trip it.
	assert.Contains(t, tu["id"].(string), "__thought__")
}

// TestPrepareGemini_FromAnthropic_ToolUseSignatureSurvivesUnknownFieldStripping
// is the load-bearing regression test for Gemini 3.x multi-turn tool use
// when the client (Claude Code) drops the off-spec thought_signature field
// from a tool_use content block on deserialization. The signature must still
// be smuggled back to Gemini via the id channel.
func TestPrepareGemini_FromAnthropic_ToolUseSignatureSurvivesUnknownFieldStripping(t *testing.T) {
	// Turn 1: Gemini → Anthropic response embeds the signature into the
	// synthesized tool_use.id.
	geminiResp := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[
			{"functionCall":{"name":"bash","args":{"command":"ls"}},
			 "thoughtSignature":"OPAQUE_GEMINI_SIG"}
		]},"finishReason":"STOP"}]
	}`)
	anthropicOut, err := translate.GeminiToAnthropicResponse(geminiResp, "gemini-3.1-pro-preview")
	require.NoError(t, err)
	resp := mustUnmarshal(t, anthropicOut)
	tu := resp["content"].([]any)[0].(map[string]any)
	smuggledID := tu["id"].(string)
	require.Contains(t, smuggledID, "__thought__")

	// Simulate Claude Code stripping the off-spec field on deserialization.
	delete(tu, "thought_signature")

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
	assert.Equal(t, "OPAQUE_GEMINI_SIG", p["thoughtSignature"])
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

func TestPrepareGemini_StripsJSONSchemaFieldsGoogleRejects(t *testing.T) {
	// Regression: Claude Code tool definitions include JSON Schema fields
	// ($schema, additionalProperties, propertyNames) that Google rejects with
	// 400. Hit production on every tools-bearing Gemini request.
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

	// Survivors: type, properties, required, description on leaf nodes.
	assert.Equal(t, "object", params["type"])
	assert.NotNil(t, params["properties"])
	assert.Equal(t, []any{"url"}, params["required"])

	// Casualties: keys Google rejects with "Cannot find field".
	assert.NotContains(t, params, "$schema", "$schema must be stripped at every level")
	assert.NotContains(t, params, "additionalProperties", "additionalProperties must be stripped at every level")

	// Nested objects: stripping must apply at every level.
	props := params["properties"].(map[string]any)
	paramsField := props["params"].(map[string]any)
	assert.NotContains(t, paramsField, "additionalProperties",
		"additionalProperties on a nested schema must also be stripped")
	assert.NotContains(t, paramsField, "propertyNames",
		"propertyNames must be stripped — Google doesn't recognize it")
	// Nested leaf description/type pass through.
	url := props["url"].(map[string]any)
	assert.Equal(t, "string", url["type"])
	assert.Equal(t, "URL to fetch", url["description"])
}

func TestPrepareGemini_StripsVendorExtensionAndDollarPrefixedKeys(t *testing.T) {
	// Regression: MCP tool schemas derived from Google APIs (and friends)
	// embed vendor extensions like `x-google-enum-descriptions` at every
	// nesting level. Gemini's proto validator rejects any unknown field
	// with 400 "Cannot find field". Strip anything with an "x-" or "$"
	// prefix instead of maintaining a moving allowlist.
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
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	params := mustUnmarshal(t, prep.Body)["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)

	assert.NotContains(t, params, "x-google-resource", "top-level x- extension must be stripped")
	assert.NotContains(t, params, "$schema", "top-level $-prefixed key must be stripped")
	assert.NotContains(t, params, "$id", "top-level $-prefixed key must be stripped")

	props := params["properties"].(map[string]any)
	rangeField := props["range"].(map[string]any)
	assert.NotContains(t, rangeField, "x-google-enum-descriptions", "nested x- extension must be stripped")
	assert.Equal(t, []any{"A1", "B2"}, rangeField["enum"], "real enum must survive")

	nested := props["nested"].(map[string]any)
	assert.NotContains(t, nested, "x-google-foo", "nested x- extension must be stripped at every level")
	leaf := nested["properties"].(map[string]any)["leaf"].(map[string]any)
	assert.NotContains(t, leaf, "x-vendor-thing", "leaf x- extension must be stripped")
	assert.Equal(t, "string", leaf["type"], "real type must survive")
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
					"path":{"type":"string","format":"uri"},
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
	assert.Equal(t, "uri", props["path"].(map[string]any)["format"], "format must survive")
	assert.Equal(t, []any{"replace", "append"}, props["mode"].(map[string]any)["enum"], "enum must survive")
	assert.Equal(t, true, props["line"].(map[string]any)["nullable"], "nullable must survive")
	assert.Equal(t, []any{"path", "mode"}, params["required"], "required must survive")
}

func TestPrepareGemini_CollapsesItemsBool(t *testing.T) {
	// Regression: MCP tool schemas (e.g. SigNoz) use `"items": true` (JSON Schema:
	// any items allowed). Gemini's proto Schema.items is optional Schema and
	// rejects boolean values with "Invalid value at ... (Schema), true".
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
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	args := props["args"].(map[string]any)
	assert.Equal(t, "array", args["type"])
	items, ok := args["items"].(map[string]any)
	require.True(t, ok, "items:true must become empty Schema, not remain bool")
	assert.NotNil(t, items)

	// items:false is removed, then repaired as missing-items-on-array → default.
	disabled := props["disabled"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string"}, disabled["items"], "items:false removed then repaired with default")

	links := props["links"].(map[string]any)
	assert.Equal(t, map[string]any{"type": "string"}, links["items"], "real items Schema must survive")
}

func TestPrepareGemini_CollapsesArrayTypeField(t *testing.T) {
	// Regression: MCP tool schemas (e.g. Pylon) use JSON Schema array-typed
	// "type" like ["array","null"]. Gemini's proto Schema expects type to be a
	// single enum value with a separate "nullable" boolean. Without collapsing,
	// Gemini rejects with "Proto field is not repeating, cannot start list."
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

func TestPrepareGemini_RepairsArrayMissingItems(t *testing.T) {
	// Regression: production Claude Code traffic includes MCP tools whose schemas
	// declare `{"type":"array"}` with no `items` field (valid JSON Schema, invalid
	// Gemini function-call schema). Gemini rejected the whole request with
	// "GenerateContentRequest.tools[0].function_declarations[N].parameters.
	// properties[X].items: missing field". Inject a permissive default instead.
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
	require.Contains(t, toolsField, "items", "top-level array missing items must get a default injected")
	assert.Equal(t, map[string]any{"type": "string"}, toolsField["items"])

	nestedItems := props["nested"].(map[string]any)["properties"].(map[string]any)["items"].(map[string]any)
	assert.Equal(t, "array", nestedItems["type"])
	require.Contains(t, nestedItems, "items", "nested array missing items must get a default injected too")
	assert.Equal(t, map[string]any{"type": "string"}, nestedItems["items"])
}

func TestPrepareGemini_DropsEmptyStringEnumEntries(t *testing.T) {
	// Regression: MCP tool schemas occasionally include `""` as an enum value
	// (e.g. an "operator" field that allows "no filter"). Gemini rejects with
	// "function_declarations[N].parameters.properties[X].enum[0]: cannot be
	// empty". Drop empty strings; drop the enum entirely when nothing remains.
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
	assert.Equal(t, []any{"eq", "neq", "gt"}, operator["enum"], "empty string must be filtered out")

	allEmpty := props["all_empty"].(map[string]any)
	assert.NotContains(t, allEmpty, "enum", "enum with only empty strings must be dropped entirely")

	normal := props["normal"].(map[string]any)
	assert.Equal(t, []any{"a", "b"}, normal["enum"], "well-formed enums must pass through unchanged")
}

func TestPrepareGemini_UserDefinedPropertyNamedProperties(t *testing.T) {
	// A user-defined property named "properties" must not be mistaken for the
	// JSON Schema "properties" keyword. Its value schema must still be filtered.
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
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	props := params["properties"].(map[string]any)

	userProp, ok := props["properties"].(map[string]any)
	require.True(t, ok, "user-defined 'properties' property must survive")
	assert.Equal(t, "object", userProp["type"])
	assert.Equal(t, "Additional properties", userProp["description"])
	assert.NotContains(t, userProp, "additionalProperties",
		"additionalProperties must be stripped even inside a property named 'properties'")
}

func TestPrepareGemini_GeminiFormatSanitizesTools(t *testing.T) {
	// The same-format (FormatGemini) path must also sanitize tool schemas.
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
	assert.NotContains(t, out, "model")
	assert.NotContains(t, out, "stream")
	params := out["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)["parameters"].(map[string]any)
	assert.NotContains(t, params, "additionalProperties", "additionalProperties must be stripped in Gemini-format path")
	assert.NotNil(t, params["properties"])
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
