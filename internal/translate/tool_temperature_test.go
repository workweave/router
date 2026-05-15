package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// temperatureField returns (value, present) from an emitted OpenAI body.
func temperatureField(t *testing.T, body []byte) (float64, bool) {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	raw, ok := doc["temperature"]
	if !ok {
		return 0, false
	}
	v, ok := raw.(float64)
	require.True(t, ok, "temperature must be numeric")
	return v, true
}

const editToolJSON = `[{"name":"Edit","description":"edit","input_schema":{"type":"object","properties":{"x":{"type":"string"}}}}]`
const editToolOpenAIJSON = `[{"type":"function","function":{"name":"Edit","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}]`

func TestToolTemperature_AnthropicSource_ForcesZeroForDeepSeekWithTools(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":` + editToolJSON + `,
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	temp, present := temperatureField(t, out.Body)
	require.True(t, present, "deepseek/* with tools and no client temp must get temperature override")
	assert.Equal(t, 0.0, temp)
}

func TestToolTemperature_AnthropicSource_ClientTempWins(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":` + editToolJSON + `,
		"temperature":0.7,
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	temp, present := temperatureField(t, out.Body)
	require.True(t, present)
	assert.Equal(t, 0.7, temp, "client-set temperature must override the deepseek default-to-zero")
}

func TestToolTemperature_AnthropicSource_NoOverrideWithoutTools(t *testing.T) {
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	_, present := temperatureField(t, out.Body)
	assert.False(t, present, "no tools means no override")
}

func TestToolTemperature_AnthropicSource_NoOverrideForOtherModels(t *testing.T) {
	cases := []string{"gpt-5", "moonshotai/kimi-k2.5", "qwen/qwen3-max", "google/gemini-2.5-pro"}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			src := []byte(`{
				"model":"claude-opus-4-7",
				"messages":[{"role":"user","content":"hi"}],
				"tools":` + editToolJSON + `,
				"max_tokens":256
			}`)
			env, err := translate.ParseAnthropic(src)
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: model})
			require.NoError(t, err)

			_, present := temperatureField(t, out.Body)
			assert.False(t, present, "non-deepseek targets must not receive a temperature override")
		})
	}
}

func TestToolTemperature_OpenAISource_ForcesZeroForDeepSeekWithTools(t *testing.T) {
	src := []byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"hi"}],
		"tools":` + editToolOpenAIJSON + `,
		"max_tokens":256
	}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	temp, present := temperatureField(t, out.Body)
	require.True(t, present, "OpenAI source path must also apply the override")
	assert.Equal(t, 0.0, temp)
}

func TestToolTemperature_OpenAISource_ClientTempWins(t *testing.T) {
	src := []byte(`{
		"model":"x",
		"messages":[{"role":"user","content":"hi"}],
		"tools":` + editToolOpenAIJSON + `,
		"temperature":0.5,
		"max_tokens":256
	}`)
	env, err := translate.ParseOpenAI(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	temp, present := temperatureField(t, out.Body)
	require.True(t, present)
	assert.Equal(t, 0.5, temp)
}
