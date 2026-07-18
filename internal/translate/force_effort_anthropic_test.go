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

// TestForceEffort_AnthropicOverrides verifies ForceEffort overrides inbound
// output_config.effort on adaptive targets, with per-model xhigh cap applied.
func TestForceEffort_AnthropicOverrides(t *testing.T) {
	cases := []struct {
		name    string
		level   string
		target  string
		wantEff string
		noLower bool // true when target accepts xhigh and shouldn't clamp
	}{
		{
			name:    "low_overrides_high_on_opus_xhigh",
			level:   "low",
			target:  "claude-opus-4-8",
			wantEff: "low",
		},
		{
			name:    "max_overrides_high_on_opus_xhigh",
			level:   "max",
			target:  "claude-opus-4-8",
			wantEff: "max",
		},
		{
			name:    "xhigh_passes_on_capable",
			level:   "xhigh",
			target:  "claude-opus-4-8",
			wantEff: "xhigh",
		},
		{
			name:    "xhigh_clamps_on_sonnet",
			level:   "xhigh",
			target:  "claude-sonnet-4-6",
			wantEff: "max",
		},
		{
			name:    "ultra_alias_resolves_to_xhigh_pass",
			level:   "ultra",
			target:  "claude-opus-4-8",
			wantEff: "xhigh",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":31999},"output_config":{"effort":"high"}}`)
			env, err := translate.ParseAnthropic(body)
			require.NoError(t, err)
			prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
				TargetModel:  tc.target,
				Capabilities: router.Lookup(tc.target),
				ForceEffort:  tc.level,
			})
			require.NoError(t, err)
			var out map[string]any
			require.NoError(t, json.Unmarshal(prep.Body, &out))
			oc, ok := out["output_config"].(map[string]any)
			require.True(t, ok, "output_config must survive the rewrite")
			assert.Equal(t, tc.wantEff, oc["effort"])
		})
	}
}

// TestForceEffort_AnthropicPassesThroughAlias verifies alias forms (e.g. ultra)
// flow end-to-end from ForceEffort into the wire output_config.effort.
func TestForceEffort_AnthropicPassesThroughAlias(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:  "claude-opus-4-7",
		Capabilities: router.Lookup("claude-opus-4-7"),
		ForceEffort:  "ultra", // aliases via CanonicalizeEffort to xhigh
	})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	oc, ok := out["output_config"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "xhigh", oc["effort"])
}

// TestForceEffort_NonAdaptiveTargetNoOp verifies ForceEffort is a no-op on
// legacy extended-thinking targets (no output_config.effort knob).
func TestForceEffort_NonAdaptiveTargetNoOp(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":16384}}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:  "claude-sonnet-4-5",
		Capabilities: router.Lookup("claude-sonnet-4-5"),
		ForceEffort:  "low",
	})
	require.NoError(t, err)
	// Use of Lookup on a sonnet-4-5 returns zero (non-adaptive) — no rewrite.
	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	// thinking block should remain "enabled" with the original budget; no
	// output_config should appear (only adaptive models use it).
	if oc, present := out["output_config"]; present {
		assert.Nil(t, oc, "non-adaptive target must not gain output_config.effort")
	}
}
