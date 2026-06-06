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

// An Anthropic client (Claude Code) drives reasoning via `thinking`, not
// `reasoning_effort`. The router must translate the thinking budget into the
// OpenAI reasoning control for reasoning-capable targets, else the upstream
// runs at its minimal default (the gpt-5.5 lobotomy).
func TestAnthropicThinkingMapsToOpenAIReasoningEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{2048, "low"},
		{8192, "medium"},
		{31999, "high"},
	}
	for _, tc := range cases {
		body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":` +
			itoa(tc.budget) + `}}`)
		env, err := translate.ParseAnthropic(body)
		require.NoError(t, err)
		p, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
			TargetModel:  "gpt-5.5",
			Capabilities: router.Lookup("gpt-5.5"),
		})
		require.NoError(t, err)
		var out map[string]any
		require.NoError(t, json.Unmarshal(p.Body, &out))
		assert.Equal(t, tc.want, out["reasoning_effort"], "budget %d", tc.budget)
		_, hasThinking := out["thinking"]
		assert.False(t, hasThinking, "thinking must be stripped from the OpenAI body")
	}
}

// Non-reasoning OpenAI targets (e.g. gpt-4.1) must NOT get a reasoning_effort —
// they reject it.
func TestAnthropicThinkingNotMappedForNonReasoningTarget(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	p, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:  "gpt-4.1",
		Capabilities: router.Lookup("gpt-4.1"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(p.Body, &out))
	_, has := out["reasoning_effort"]
	assert.False(t, has, "non-reasoning target must not receive reasoning_effort")
}

// A client that already specified reasoning_effort wins — the thinking-derived
// value must not overwrite it.
func TestExplicitReasoningEffortNotOverwrittenByThinking(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low","thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	p, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:  "gpt-5.5",
		Capabilities: router.Lookup("gpt-5.5"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(p.Body, &out))
	assert.Equal(t, "low", out["reasoning_effort"], "caller-supplied reasoning_effort must win")
}

// The same thinking budget must reach a native-Gemini target as a
// thinkingConfig.thinkingBudget, so gemini-3.x reasons instead of defaulting.
func TestAnthropicThinkingMapsToGeminiThinkingConfig(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	p, err := env.PrepareGemini(nil, translate.EmitOptions{
		TargetModel:  "gemini-3.1-pro-preview",
		Capabilities: router.Lookup("gemini-3.1-pro-preview"),
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(p.Body, &out))
	gen, ok := out["generationConfig"].(map[string]any)
	require.True(t, ok, "generationConfig must be present")
	tc, ok := gen["thinkingConfig"].(map[string]any)
	require.True(t, ok, "thinkingConfig must be set from the thinking budget")
	assert.EqualValues(t, 24576, tc["thinkingBudget"], "high budget -> gemini high thinkingBudget")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
