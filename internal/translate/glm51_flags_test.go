package translate_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GLM-5.1 ships the fix for the empty-input tool_call loop GLM-5 exhibits, but
// the fix is opt-in via tool_stream=true (Z.AI streaming docs). The router
// always opts in. Thinking mode is also disabled on the DeepInfra path via the
// vLLM template kwarg; OpenRouter handles the same disable through its native
// reasoning hint. See docs/investigations/2026-05-26-glm5-empty-tool-loop.md.

func TestGLM51Flags_DeepInfra_OpenAISameFormat(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "z-ai/glm-5.1",
		TargetProvider: providers.ProviderDeepInfra,
		Capabilities:   router.Lookup("z-ai/glm-5.1"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, true, out["tool_stream"], "glm-5.1 must receive tool_stream=true on DeepInfra")
	kwargs, ok := out["chat_template_kwargs"].(map[string]any)
	require.True(t, ok, "glm-5.1 on DeepInfra must carry chat_template_kwargs object")
	assert.Equal(t, false, kwargs["enable_thinking"], "glm-5.1 on DeepInfra must disable thinking via chat_template_kwargs")
}

func TestGLM51Flags_DeepInfra_AnthropicCrossFormat(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 256,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "z-ai/glm-5.1",
		TargetProvider: providers.ProviderDeepInfra,
		Capabilities:   router.Lookup("z-ai/glm-5.1"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, true, out["tool_stream"], "anthropic→deepinfra glm-5.1 must receive tool_stream=true")
	kwargs, ok := out["chat_template_kwargs"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, kwargs["enable_thinking"])
}

func TestGLM51Flags_OpenRouter_NoTemplateKwargs(t *testing.T) {
	// OpenRouter uses its own reasoning={enabled:false} hint (added to
	// openRouterReasoningHint); the chat_template_kwargs path is DeepInfra-
	// specific and must not appear on OpenRouter requests.
	body := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 256,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "z-ai/glm-5.1",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("z-ai/glm-5.1"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, true, out["tool_stream"], "openrouter glm-5.1 still gets tool_stream=true")
	_, hasKwargs := out["chat_template_kwargs"]
	assert.False(t, hasKwargs, "openrouter path must not set chat_template_kwargs")
	reasoning, ok := out["reasoning"].(map[string]any)
	require.True(t, ok, "openrouter glm-5.1 must carry reasoning hint")
	assert.Equal(t, false, reasoning["enabled"], "openrouter glm-5.1 reasoning must be disabled")
}

func TestGLM51Flags_ClientSetToolStreamPreserved(t *testing.T) {
	// Client-supplied tool_stream wins so callers can opt out for debugging.
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tool_stream":false}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "z-ai/glm-5.1",
		TargetProvider: providers.ProviderDeepInfra,
		Capabilities:   router.Lookup("z-ai/glm-5.1"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, false, out["tool_stream"], "client-set tool_stream=false must be preserved")
}

func TestGLM51Flags_NotAppliedToOtherModels(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "z-ai/glm-5",
		TargetProvider: providers.ProviderDeepInfra,
		Capabilities:   router.Lookup("z-ai/glm-5"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	_, hasToolStream := out["tool_stream"]
	assert.False(t, hasToolStream, "glm-5 (not 5.1) must not receive tool_stream injection")
	_, hasKwargs := out["chat_template_kwargs"]
	assert.False(t, hasKwargs, "glm-5 (not 5.1) must not receive chat_template_kwargs")
}
