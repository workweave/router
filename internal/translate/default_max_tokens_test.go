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

// Anthropic Messages requires max_tokens; we inject a per-model default when
// absent. defaultMaxOutputTokenCap is 8192, floored by per-model caps.

func TestAnthropicSameFormat_DefaultMaxTokensInjectedWhenAbsent(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, float64(8192), out["max_tokens"])
}

func TestAnthropicSameFormat_ExistingMaxTokensUnchanged(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`)
	opts := translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	}
	out := parseAndEmit(t, body, "anthropic", opts)
	assert.Equal(t, float64(1024), out["max_tokens"])
}

func TestOpenAISameFormat_DefaultMaxTokensInjectedForNonReasoningTarget(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4o",
		Capabilities: router.Lookup("gpt-4o"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(8192), out["max_tokens"])
	assert.NotContains(t, out, "max_completion_tokens")
}

func TestOpenAISameFormat_DefaultMaxCompletionTokensInjectedForReasoningTarget(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "o3",
		Capabilities: router.Lookup("o3"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(8192), out["max_completion_tokens"])
	assert.NotContains(t, out, "max_tokens")
}

// gpt-4-turbo's cap (4096) is below the global default (8192); default must
// floor to the model cap so we never inject a value the model rejects.
func TestOpenAISameFormat_DefaultRespectsLowerPerModelCap(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4-turbo",
		Capabilities: router.Lookup("gpt-4-turbo"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(4096), out["max_tokens"])
}

// gpt-4.1 caps at 32768 above the global default (8192); the global cap is
// the binding floor for the default.
func TestOpenAISameFormat_DefaultCappedByGlobalCap(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(8192), out["max_tokens"])
}

func TestOpenAISameFormat_DefaultNotInjectedWhenMaxTokensPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":512}`)
	opts := translate.EmitOptions{
		TargetModel:  "gpt-4o",
		Capabilities: router.Lookup("gpt-4o"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(512), out["max_tokens"])
}

func TestOpenAISameFormat_DefaultNotInjectedWhenMaxCompletionTokensPresent(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":512}`)
	opts := translate.EmitOptions{
		TargetModel:  "o3",
		Capabilities: router.Lookup("o3"),
	}
	out := parseAndEmit(t, body, "openai", opts)
	assert.Equal(t, float64(512), out["max_completion_tokens"])
	assert.NotContains(t, out, "max_tokens")
}

// Regression: pullMaxTokens previously hardcoded 4096; the per-model default replaces it.
func TestCrossFormat_OpenAIToAnthropic_DefaultMaxTokensInjectedWhenAbsent(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, float64(8192), out["max_tokens"])
}

// Source omits max_tokens, non-reasoning target: injection populates max_tokens.
func TestCrossFormat_AnthropicToOpenAI_DefaultMaxTokensInjectedWhenAbsent(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:  "gpt-4o",
		Capabilities: router.Lookup("gpt-4o"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, float64(8192), out["max_tokens"])
}

// Reasoning target: injection then rename to max_completion_tokens.
func TestCrossFormat_AnthropicToOpenAI_DefaultMaxCompletionTokensForReasoning(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:  "o3",
		Capabilities: router.Lookup("o3"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	assert.Equal(t, float64(8192), out["max_completion_tokens"])
	assert.NotContains(t, out, "max_tokens")
}

// Invariant: default-injection must not mutate the source body bytes.
func TestAnthropicSameFormat_DefaultInjectionPreservesSourceBytes(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-20250514","messages":[{"role":"user","content":"hi"}]}`)
	original := make([]byte, len(body))
	copy(original, body)

	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	_, err = env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
	})
	require.NoError(t, err)

	assert.Equal(t, original, body)
}
