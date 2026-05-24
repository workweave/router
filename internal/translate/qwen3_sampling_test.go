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

// Qwen3 anti-loop sampling: the router injects presence_penalty=1.5 when an
// OpenRouter-bound request targets a qwen3 model and the client did not set
// one itself. Validates both the OpenAI same-format path and the
// Anthropic→OpenAI cross-format path.

func TestQwen3PresencePenalty_OpenAISameFormat_InjectedForQwen3(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
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
	assert.Equal(t, 1.5, out["presence_penalty"], "qwen3 must receive presence_penalty=1.5")
}

func TestQwen3PresencePenalty_NotInjectedForNonQwen(t *testing.T) {
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
	_, has := out["presence_penalty"]
	assert.False(t, has, "non-qwen3 models must not receive the presence_penalty override")
}

func TestQwen3PresencePenalty_DoesNotOverrideClientValue(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"presence_penalty":0.2}`)
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
	assert.Equal(t, 0.2, out["presence_penalty"], "client-set presence_penalty must win over the qwen3 default")
}

func TestQwen3PresencePenalty_AnthropicToOpenAI(t *testing.T) {
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
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, 1.5, out["presence_penalty"], "cross-format emit must inject qwen3 presence_penalty")
}
