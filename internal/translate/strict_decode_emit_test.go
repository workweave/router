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

// Prevention layer for strongly-typed tool calls: upstreams that expose a
// decode-time constraint knob get it turned on, so out-of-schema tool calls
// stop being generated at the source instead of being repaired after the
// fact.
//   - OpenAI Responses (gpt-5.x): tools[].strict=true + strictified schema.
//   - Gemini 3.x: toolConfig.functionCallingConfig.mode=VALIDATED.

const anthropicToolsRequest = `{
  "model":"claude-opus-4-8","max_tokens":4096,
  "tools":[{"name":"Read","description":"read a file","input_schema":{
	"type":"object",
	"properties":{"file_path":{"type":"string"},"pages":{"type":"string"}},
	"required":["file_path"]
  }}],
  "messages":[{"role":"user","content":"read main.go"}]
}`

func TestPrepareOpenAIResponses_StrictTools(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(anthropicToolsRequest))
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
		TargetModel:  "gpt-5.5",
		Capabilities: router.Lookup("gpt-5.5"),
	})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	tools, _ := out["tools"].([]any)
	require.Len(t, tools, 1)
	tool0, _ := tools[0].(map[string]any)

	assert.Equal(t, true, tool0["strict"],
		"a strictifiable schema must opt into grammar-constrained decoding")
	params, _ := tool0["parameters"].(map[string]any)
	require.NotNil(t, params)
	assert.Equal(t, false, params["additionalProperties"])
	assert.ElementsMatch(t, []any{"file_path", "pages"}, params["required"].([]any),
		"strict mode requires every property in required")
	pages := params["properties"].(map[string]any)["pages"].(map[string]any)
	assert.Equal(t, []any{"string", "null"}, pages["type"],
		"the optional param is expressed as a null union, not omission")
}

func TestPrepareOpenAIResponses_NonStrictifiableFallsBack(t *testing.T) {
	// oneOf is outside the strict subset: the tool must emit its ORIGINAL
	// schema without strict rather than fail or mangle it.
	body := `{
	  "model":"claude-opus-4-8","max_tokens":4096,
	  "tools":[{"name":"Pick","input_schema":{
	    "type":"object",
	    "properties":{"choice":{"oneOf":[{"type":"string"},{"type":"integer"}]}},
	    "required":["choice"]
	  }}],
	  "messages":[{"role":"user","content":"pick"}]
	}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
		TargetModel:  "gpt-5.5",
		Capabilities: router.Lookup("gpt-5.5"),
	})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	tools, _ := out["tools"].([]any)
	require.Len(t, tools, 1)
	tool0, _ := tools[0].(map[string]any)

	assert.Equal(t, false, tool0["strict"])
	params, _ := tool0["parameters"].(map[string]any)
	require.NotNil(t, params)
	assert.NotContains(t, params, "additionalProperties",
		"the original schema is emitted untouched on fallback")
	choice := params["properties"].(map[string]any)["choice"].(map[string]any)
	assert.Contains(t, choice, "oneOf")
}

func TestPrepareGemini_ValidatedModeOnGemini3x(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(anthropicToolsRequest))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	tc := getMap(t, doc, "toolConfig")
	fcc := tc["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "VALIDATED", fcc["mode"],
		"tools present + no forced tool_choice → schema-constrained decoding without forcing a call")
}

func TestPrepareGemini_ValidatedModeNotOnLegacyModels(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(anthropicToolsRequest))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	assert.NotContains(t, doc, "toolConfig",
		"non-3.x targets keep the legacy behavior: no toolConfig when tool_choice is absent")
}

func TestPrepareGemini_ForcedToolChoicePreserved(t *testing.T) {
	// An explicit client tool_choice must never be clobbered by the
	// VALIDATED upgrade.
	body := `{
	  "model":"claude-opus-4-8","max_tokens":4096,
	  "tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
	  "tool_choice":{"type":"tool","name":"Read"},
	  "messages":[{"role":"user","content":"read main.go"}]
	}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "ANY", fcc["mode"])
	assert.Equal(t, []any{"Read"}, fcc["allowedFunctionNames"].([]any))
}

func TestPrepareGemini_ValidatedModeFromOpenAIIngress(t *testing.T) {
	// The OpenAI→Gemini cross-format path must request VALIDATED under the
	// same conditions as the Anthropic path (PR #343 review).
	body := `{
	  "model":"gpt-4o","max_tokens":1024,
	  "tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}],
	  "messages":[{"role":"user","content":"read main.go"}]
	}`
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "VALIDATED", fcc["mode"])
}

func TestPrepareGemini_OpenAIIngressForcedChoicePreserved(t *testing.T) {
	body := `{
	  "model":"gpt-4o","max_tokens":1024,
	  "tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}],
	  "tool_choice":"required",
	  "messages":[{"role":"user","content":"read main.go"}]
	}`
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "ANY", fcc["mode"], "an explicit tool_choice must never be clobbered by the VALIDATED upgrade")
}

func TestPrepareGemini_NoToolsNoValidated(t *testing.T) {
	body := `{
	  "model":"claude-opus-4-8","max_tokens":4096,
	  "messages":[{"role":"user","content":"hello"}]
	}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	assert.NotContains(t, doc, "toolConfig", "no tools → nothing to constrain")
	assert.False(t, prep.Stats.GeminiValidatedToolMode, "no tools → VALIDATED was not emitted")
}

func TestPrepareGemini_ValidatedModeReportedInStats(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(anthropicToolsRequest))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	assert.True(t, prep.Stats.GeminiValidatedToolMode,
		"a VALIDATED-mode emission must be reported so the proxy can decide on an AUTO retry")
}

func TestPrepareGemini_DowngradeValidatedToAuto(t *testing.T) {
	// The proxy sets DowngradeGeminiValidatedToAuto on a retry after a
	// VALIDATED-mode INVALID_ARGUMENT 400. The same tools-with-no-forced-choice
	// request must now emit mode=AUTO so Gemini skips schema-grammar compilation.
	env, err := translate.ParseAnthropic([]byte(anthropicToolsRequest))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:                    "gemini-3.1-pro-preview",
		DowngradeGeminiValidatedToAuto: true,
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "AUTO", fcc["mode"], "the downgrade replaces VALIDATED with AUTO")
	assert.False(t, prep.Stats.GeminiValidatedToolMode,
		"once downgraded the request no longer uses VALIDATED, so a second retry must not fire")
}

func TestPrepareGemini_DowngradeValidatedToAutoFromOpenAIIngress(t *testing.T) {
	body := `{
	  "model":"gpt-4o","max_tokens":1024,
	  "tools":[{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}}],
	  "messages":[{"role":"user","content":"read main.go"}]
	}`
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:                    "gemini-3.1-pro-preview",
		DowngradeGeminiValidatedToAuto: true,
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "AUTO", fcc["mode"])
}

func TestPrepareGemini_DowngradeNoOpWhenForcedChoice(t *testing.T) {
	// A forced tool_choice never went out as VALIDATED, so the downgrade flag is
	// a no-op: the explicit choice is preserved and no retry is signalled.
	body := `{
	  "model":"claude-opus-4-8","max_tokens":4096,
	  "tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
	  "tool_choice":{"type":"tool","name":"Read"},
	  "messages":[{"role":"user","content":"read main.go"}]
	}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:                    "gemini-3.1-pro-preview",
		DowngradeGeminiValidatedToAuto: true,
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	fcc := getMap(t, doc, "toolConfig")["functionCallingConfig"].(map[string]any)
	assert.Equal(t, "ANY", fcc["mode"], "a forced tool_choice is untouched by the downgrade")
	assert.False(t, prep.Stats.GeminiValidatedToolMode)
}
