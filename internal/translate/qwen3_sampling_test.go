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

// Qwen3 sampling defaults: the router layers the Qwen3 model-card recommended
// sampling parameters onto outbound bodies when the target is a qwen3 model
// and the client did not set the corresponding field. Applies across all
// OpenAI-compat backends (OpenRouter, Bedrock, DeepInfra, Fireworks) because
// the recommendation is model-keyed, not provider-keyed.

const qwen3PresencePenaltyExpected = 1.5
const qwen3TemperatureExpected = 0.7
const qwen3TopPExpected = 0.8
const qwen3RepetitionPenaltyExpected = 1.05

func assertQwen3Defaults(t *testing.T, body []byte) {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, qwen3TemperatureExpected, out["temperature"], "qwen3 must receive temperature=0.7")
	assert.Equal(t, qwen3TopPExpected, out["top_p"], "qwen3 must receive top_p=0.8")
	assert.Equal(t, qwen3PresencePenaltyExpected, out["presence_penalty"], "qwen3 must receive presence_penalty=1.5")
	assert.Equal(t, qwen3RepetitionPenaltyExpected, out["repetition_penalty"], "qwen3 must receive repetition_penalty=1.05")
}

func TestQwen3Samplers_OpenAISameFormat_InjectedForQwen3(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "qwen/qwen3.6-35b-a3b",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("qwen/qwen3.6-35b-a3b"),
	})
	require.NoError(t, err)
	assertQwen3Defaults(t, prep.Body)
}

func TestQwen3Samplers_AnthropicCrossFormat_InjectedForQwen3(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 256,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "qwen/qwen3.6-35b-a3b",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("qwen/qwen3.6-35b-a3b"),
	})
	require.NoError(t, err)
	assertQwen3Defaults(t, prep.Body)
}

func TestQwen3Samplers_AppliedOnBedrockTarget(t *testing.T) {
	// Production bedrock-mantle traffic was previously skipping the qwen3
	// sampler block because the targetIsOpenRouter gate excluded it. With
	// the model-keyed application, samplers now reach Bedrock too.
	body := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 256,
		"messages": [{"role":"user","content":"hi"}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "qwen/qwen3-235b-a22b-2507",
		TargetProvider: providers.ProviderBedrock,
		Capabilities:   router.Lookup("qwen/qwen3-235b-a22b-2507"),
	})
	require.NoError(t, err)
	assertQwen3Defaults(t, prep.Body)
}

func TestQwen3Samplers_NotInjectedForNonQwen(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "deepseek/deepseek-v4-pro",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("deepseek/deepseek-v4-pro"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	for _, key := range []string{"top_p", "presence_penalty", "repetition_penalty"} {
		_, has := out[key]
		assert.False(t, has, "non-qwen3 models must not receive the %s default", key)
	}
}

func TestQwen3Samplers_DoNotOverrideClientValues(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o",
		"messages":[{"role":"user","content":"hi"}],
		"temperature":0.1,
		"top_p":0.5,
		"presence_penalty":0.2,
		"repetition_penalty":1.2
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "qwen/qwen3.6-35b-a3b",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("qwen/qwen3.6-35b-a3b"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, 0.1, out["temperature"], "client temperature must win")
	assert.Equal(t, 0.5, out["top_p"], "client top_p must win")
	assert.Equal(t, 0.2, out["presence_penalty"], "client presence_penalty must win")
	assert.Equal(t, 1.2, out["repetition_penalty"], "client repetition_penalty must win")
}

func TestQwen3Samplers_PartialClientOverridesFillRest(t *testing.T) {
	// Client sets only temperature; the router should still fill the other
	// three defaults but leave temperature alone.
	body := []byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 256,
		"messages": [{"role":"user","content":"hi"}],
		"temperature": 0.3
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "qwen/qwen3.6-35b-a3b",
		TargetProvider: providers.ProviderOpenRouter,
		Capabilities:   router.Lookup("qwen/qwen3.6-35b-a3b"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, 0.3, out["temperature"], "client temperature must win")
	assert.Equal(t, qwen3TopPExpected, out["top_p"])
	assert.Equal(t, qwen3PresencePenaltyExpected, out["presence_penalty"])
	assert.Equal(t, qwen3RepetitionPenaltyExpected, out["repetition_penalty"])
}
