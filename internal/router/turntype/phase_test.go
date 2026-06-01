package turntype_test

import (
	"testing"

	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectPhase_Anthropic(t *testing.T) {
	tests := []struct {
		name string
		body string
		hint string
		want turntype.Phase
	}{
		{
			name: "plan mode via system reminder is planning",
			body: `{"model":"claude-opus-4-7","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"system":"You are Claude Code.\nPlan mode is active. The user indicated they do not want you to execute yet.","messages":[{"role":"user","content":"add a feature"}]}`,
			want: turntype.PhasePlanning,
		},
		{
			// ExitPlanMode is offered only while plan mode is active, so its
			// presence is a second independent planning signal even without
			// the system substring.
			name: "plan mode via ExitPlanMode tool is planning",
			body: `{"model":"claude-opus-4-7","tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"ExitPlanMode","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"design the change"}]}`,
			want: turntype.PhasePlanning,
		},
		{
			name: "explore sub-agent via metadata is research",
			body: `{"model":"claude-haiku-4-5","metadata":{"user_id":"subagent:Explore"},"messages":[{"role":"user","content":"find all go files"}]}`,
			want: turntype.PhaseResearch,
		},
		{
			name: "explore sub-agent via header hint is research",
			body: `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"grep"}]}`,
			hint: "Explore",
			want: turntype.PhaseResearch,
		},
		{
			name: "main loop with tools and no plan mode is none",
			body: `{"model":"claude-opus-4-7","tools":[{"name":"Edit","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"fix the bug"}]}`,
			want: turntype.PhaseNone,
		},
		{
			name: "plain user prompt is none",
			body: `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"explain recursion"}]}`,
			want: turntype.PhaseNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			tt := turntype.DetectFromEnvelope(env, feats, tc.hint)
			got := turntype.DetectPhase(env, tt)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDetectPhase_NonAnthropicPlanModeGated(t *testing.T) {
	// Plan mode is a Claude-Code-only signal and Claude Code always talks
	// Anthropic wire format. A non-Anthropic request whose content matches the
	// plan-mode signals must NOT classify as planning, mirroring how compaction
	// detection is format-gated.
	t.Run("openai system substring stays none", func(t *testing.T) {
		body := `{"model":"gpt-4o","messages":[{"role":"system","content":"Plan mode is active. Do not execute yet."},{"role":"user","content":"go"}]}`
		env, err := translate.ParseOpenAI([]byte(body))
		require.NoError(t, err)
		feats := env.RoutingFeatures(false)
		tt := turntype.DetectFromEnvelope(env, feats, "")
		assert.Equal(t, turntype.PhaseNone, turntype.DetectPhase(env, tt))
	})

	t.Run("openai ExitPlanMode tool stays none", func(t *testing.T) {
		body := `{"model":"gpt-4o","tools":[{"type":"function","function":{"name":"ExitPlanMode"}}],"messages":[{"role":"user","content":"go"}]}`
		env, err := translate.ParseOpenAI([]byte(body))
		require.NoError(t, err)
		feats := env.RoutingFeatures(false)
		tt := turntype.DetectFromEnvelope(env, feats, "")
		assert.Equal(t, turntype.PhaseNone, turntype.DetectPhase(env, tt))
	})

	t.Run("gemini header hint still research", func(t *testing.T) {
		body := `{"contents":[{"role":"user","parts":[{"text":"grep"}]}]}`
		env, err := translate.ParseGemini([]byte(body))
		require.NoError(t, err)
		feats := env.RoutingFeatures(false)
		tt := turntype.DetectFromEnvelope(env, feats, "Explore")
		assert.Equal(t, turntype.PhaseResearch, turntype.DetectPhase(env, tt))
	})
}

func TestDetectPhase_NilEnv(t *testing.T) {
	assert.Equal(t, turntype.PhaseNone, turntype.DetectPhase(nil, turntype.MainLoop))
}
