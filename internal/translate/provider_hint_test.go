package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerField extracts the `provider` field from an emitted OpenAI body.
// Returns nil if absent.
func providerField(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	p, _ := doc["provider"].(map[string]any)
	return p
}

func TestPrepareOpenAI_AnthropicSource_InjectsDeepSeekProviderHint(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:  "deepseek/deepseek-v4-pro",
		Capabilities: router.ModelSpec{},
	})
	require.NoError(t, err)

	p := providerField(t, out.Body)
	require.NotNil(t, p, "expected provider hint for deepseek/* target")
	order, _ := p["order"].([]any)
	require.Len(t, order, 1)
	assert.Equal(t, "deepseek", order[0])
	assert.Equal(t, false, p["allow_fallbacks"])
}

func TestPrepareOpenAI_AnthropicSource_InjectsMoonshotProviderHint(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "moonshotai/kimi-k2.5",
	})
	require.NoError(t, err)

	p := providerField(t, out.Body)
	require.NotNil(t, p)
	order, _ := p["order"].([]any)
	require.Len(t, order, 1)
	assert.Equal(t, "moonshotai", order[0])
	assert.Equal(t, false, p["allow_fallbacks"])
}

func TestPrepareOpenAI_OpenAISource_InjectsProviderHint(t *testing.T) {
	src := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-v4-pro",
	})
	require.NoError(t, err)

	p := providerField(t, out.Body)
	require.NotNil(t, p, "OpenAI→OpenAI path must inject the hint too")
	order, _ := p["order"].([]any)
	require.Len(t, order, 1)
	assert.Equal(t, "deepseek", order[0])
}

func TestPrepareOpenAI_NoHintForUnrecognizedModel(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "gpt-5",
	})
	require.NoError(t, err)

	assert.Nil(t, providerField(t, out.Body),
		"first-party OpenAI/Fireworks targets must not get a provider hint")
}

// reasoningField extracts the `reasoning` field from an emitted OpenAI body.
func reasoningField(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	r, _ := doc["reasoning"].(map[string]any)
	return r
}

func TestPrepareOpenAI_AnthropicSource_DisablesReasoningForDeepSeek(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-v4-pro",
	})
	require.NoError(t, err)

	r := reasoningField(t, out.Body)
	require.NotNil(t, r, "deepseek/* must get a reasoning override")
	assert.Equal(t, false, r["enabled"])
}

func TestPrepareOpenAI_OpenAISource_DisablesReasoningForDeepSeek(t *testing.T) {
	src := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-v4-pro",
	})
	require.NoError(t, err)

	r := reasoningField(t, out.Body)
	require.NotNil(t, r)
	assert.Equal(t, false, r["enabled"])
}

func TestPrepareOpenAI_NoReasoningOverrideForNonDeepSeek(t *testing.T) {
	cases := []string{"moonshotai/kimi-k2.5", "qwen/qwen3-max", "google/gemini-2.5-pro", "gpt-5"}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: model})
			require.NoError(t, err)

			assert.Nil(t, reasoningField(t, out.Body),
				"non-deepseek targets must not get a reasoning override")
		})
	}
}

// SOC 2 isolation routes deepseek/moonshotai/qwen slugs to direct
// upstreams (Fireworks/DeepInfra/Bedrock) where the OpenRouter-only
// `provider` and `reasoning` body fields cause a 400. The emit path
// must gate those fields on the resolved target provider.
func TestPrepareOpenAI_AnthropicSource_SkipsHintsForNonOpenRouterProvider(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	cases := []struct {
		name     string
		target   string
		provider string
	}{
		{"fireworks dispatches deepseek", "deepseek/deepseek-v4-pro", providers.ProviderFireworks},
		{"deepinfra dispatches deepseek", "deepseek/deepseek-v4-flash", providers.ProviderDeepInfra},
		{"bedrock dispatches moonshotai", "moonshotai/kimi-k2.5", providers.ProviderBedrock},
		{"bedrock dispatches qwen", "qwen/qwen3-coder-next", providers.ProviderBedrock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
				TargetModel:    tc.target,
				TargetProvider: tc.provider,
			})
			require.NoError(t, err)

			assert.Nil(t, providerField(t, out.Body),
				"%s must not receive OpenRouter `provider` hint", tc.provider)
			assert.Nil(t, reasoningField(t, out.Body),
				"%s must not receive OpenRouter `reasoning` hint", tc.provider)
		})
	}
}

func TestPrepareOpenAI_OpenAISource_SkipsHintsForNonOpenRouterProvider(t *testing.T) {
	src := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "deepseek/deepseek-v4-pro",
		TargetProvider: providers.ProviderFireworks,
	})
	require.NoError(t, err)

	assert.Nil(t, providerField(t, out.Body))
	assert.Nil(t, reasoningField(t, out.Body))
}

func TestPrepareOpenAI_ExplicitOpenRouterProviderStillGetsHints(t *testing.T) {
	src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:    "deepseek/deepseek-v4-pro",
		TargetProvider: providers.ProviderOpenRouter,
	})
	require.NoError(t, err)

	require.NotNil(t, providerField(t, out.Body))
	require.NotNil(t, reasoningField(t, out.Body))
}

func TestPrepareOpenAI_QwenAndGoogleGetSortHint(t *testing.T) {
	cases := []string{"qwen/qwen3-max", "google/gemini-2.5-pro"}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			src := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}],"max_tokens":256}`)
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: model})
			require.NoError(t, err)

			p := providerField(t, out.Body)
			require.NotNil(t, p)
			assert.Equal(t, "throughput", p["sort"])
			assert.Nil(t, p["order"], "throughput-sort hint must not also pin order")
		})
	}
}

func TestPrepareOpenAI_OpenAISource_EmptyToolsNoOverrides(t *testing.T) {
	src := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"tools":[],"max_tokens":256}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-chat",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Body, &doc))

	_, hasTemp := doc["temperature"]
	assert.False(t, hasTemp, "empty tools must not trigger temperature override")
}
