package translate_test

import (
	"encoding/json"
	"testing"

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
