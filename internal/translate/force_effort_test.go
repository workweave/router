package translate_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
	"workweave/router/internal/translate"
)

// EmitOptions.ForceReasoningEffort overrides the request-derived effort on the
// gpt-5.x Responses path. This is the primitive the escalate-on-failure policy
// rides: serve low by default, force high after an observed failed turn.
func TestForceReasoningEffort_ResponsesOverride(t *testing.T) {
	// Inbound carries a *high* thinking budget (would resolve to "high"); the
	// override pins it to "low". And vice-versa: a tiny budget forced to "high".
	cases := []struct {
		name        string
		budget      int
		forceEffort string
		want        string
	}{
		{"force_low_over_high_budget", 31999, "low", "low"},
		{"force_high_over_low_budget", 2048, "high", "high"},
		{"empty_override_keeps_budget_low", 2048, "", "low"},
		{"empty_override_keeps_budget_high", 31999, "", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":` + itoaLocal(tc.budget) + `}}`)
			env, err := translate.ParseAnthropic(body)
			require.NoError(t, err)
			prep, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{
				TargetModel:          "gpt-5.5",
				Capabilities:         router.Lookup("gpt-5.5"),
				ForceReasoningEffort: tc.forceEffort,
			})
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			reasoning, _ := out["reasoning"].(map[string]any)
			require.NotNil(t, reasoning)
			assert.Equal(t, tc.want, reasoning["effort"])
		})
	}
}

// The same override pins gemini-3.x thinkingLevel — the policy forces gemini to
// "low" (effort-immune on hard tasks) regardless of the inbound budget.
func TestForceReasoningEffort_GeminiOverride(t *testing.T) {
	// Inbound high budget would map to thinkingLevel "high"; override forces low.
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:          "gemini-3.1-pro-preview",
		ForceReasoningEffort: "low",
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	gc := out["generationConfig"].(map[string]any)
	tc := gc["thinkingConfig"].(map[string]any)
	assert.Equal(t, "low", tc["thinkingLevel"])
}

// The override also applies on the OpenAI→Gemini cross-format path (the
// request arrives as OpenAI chat/completions and is translated to native Gemini).
func TestForceReasoningEffort_GeminiFromOpenAI(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:          "gemini-3.1-pro-preview",
		ForceReasoningEffort: "low",
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	gc := out["generationConfig"].(map[string]any)
	tc := gc["thinkingConfig"].(map[string]any)
	assert.Equal(t, "low", tc["thinkingLevel"])
}

func itoaLocal(n int) string {
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
