package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/planner"
)

func TestRoutingMarkerFor_TierClampNote(t *testing.T) {
	t.Parallel()

	t.Run("low-tier clamp names the would-have model", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-haiku-4-5", Provider: "anthropic"},
			TierClamped:   true,
			PreClampModel: "deepseek/deepseek-v4-pro",
			RequestedTier: catalog.TierLow,
		}
		got := routingMarkerFor(res)
		assert.Contains(t, got, "second-choice pick at low tier")
		assert.Contains(t, got, "(would have used deepseek/deepseek-v4-pro)")
		assert.NotContains(t, got, "to unlock", "upsell suffix dropped from the simplified note")
	})

	t.Run("mid-tier clamp names the would-have model", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-sonnet-4-5", Provider: "anthropic"},
			TierClamped:   true,
			PreClampModel: "claude-opus-4-7",
			RequestedTier: catalog.TierMid,
		}
		got := routingMarkerFor(res)
		assert.Contains(t, got, "second-choice pick at mid tier")
		assert.Contains(t, got, "(would have used claude-opus-4-7)")
		assert.NotContains(t, got, "to unlock")
	})

	t.Run("no clamp emits no note", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-opus-4-7", Provider: "anthropic"},
			TierClamped:   false,
			RequestedTier: catalog.TierHigh,
		}
		got := routingMarkerFor(res)
		assert.NotContains(t, got, "second-choice")
		assert.NotContains(t, got, "would have used")
	})
}

func TestRoutingMarkerFor_PlannerPaths(t *testing.T) {
	decision := router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "openrouter"}

	cases := []struct {
		name           string
		res            turnLoopResult
		wantContains   []string
		wantNotContain []string
		wantEmpty      bool
	}{
		{
			name: "ev_positive: planner switched",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonEVPositive},
			},
			wantContains: []string{
				"✦ **Weave Router** → deepseek/deepseek-v4-pro · " + markerReasonSwitched,
			},
			wantNotContain: []string{
				"(openrouter)",
				"reason:",
				"ev_positive",
			},
		},
		{
			name: "ev_negative: planner stayed",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonEVNegative},
			},
			wantContains: []string{
				"· " + markerReasonStayed,
			},
			wantNotContain: []string{
				"(openrouter)",
				"reason:",
				"ev_negative",
			},
		},
		{
			name: "no_prior_usage: collapses into stayed bucket",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonNoPriorUsage},
			},
			wantContains: []string{
				"· " + markerReasonStayed,
			},
			wantNotContain: []string{
				"no cache stats",
			},
		},
		{
			name: "same_model: collapses into best-pick bucket",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonSameModel},
			},
			wantContains: []string{
				"· " + markerReasonBestPick,
			},
			wantNotContain: []string{
				"scorer matches the pin",
				"reason:",
			},
		},
		{
			name: "tier_upgrade: planner bumped to a higher tier",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonTierUpgrade},
			},
			wantContains: []string{
				"· " + markerReasonTierUpgrade,
			},
		},
		{
			name: "pricing_missing: recovery code is silenced",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonPricingMissing},
			},
			wantContains: []string{
				"✦ **Weave Router** → deepseek/deepseek-v4-pro",
			},
			wantNotContain: []string{
				"·",
				"pricing",
				"missing",
			},
		},
		{
			name: "pin_model_missing: recovery code is silenced",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonPinModelMissing},
			},
			wantNotContain: []string{
				"·",
				"pin model",
			},
		},
		{
			name: "no planner ran, fresh decision: best-pick fallback",
			res: turnLoopResult{
				Decision: decision,
			},
			wantContains: []string{
				"· " + markerReasonBestPick,
			},
			wantNotContain: []string{
				"top scorer",
				"reason:",
			},
		},
		{
			name: "hard-pin path: planner didn't run, mark it accordingly",
			res: turnLoopResult{
				Decision:   decision,
				HardPinned: true,
			},
			wantContains: []string{
				"· " + markerReasonHardPinned,
			},
			wantNotContain: []string{
				"hard pin",
				"reason:",
			},
		},
		{
			name: "tool-result short-circuit: marker suppressed",
			res: turnLoopResult{
				Decision:  decision,
				StickyHit: true,
			},
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routingMarkerFor(tc.res)
			if tc.wantEmpty {
				assert.Empty(t, got)
				return
			}
			for _, want := range tc.wantContains {
				assert.Contains(t, got, want)
			}
			for _, nope := range tc.wantNotContain {
				assert.NotContains(t, got, nope)
			}
			assert.True(t, len(got) >= 2 && got[len(got)-2:] == "\n\n",
				"marker must terminate with a blank line")
		})
	}
}

func TestRoutingMarkerFor_EmptyDecisionEmitsNothing(t *testing.T) {
	got := routingMarkerFor(turnLoopResult{Decision: router.Decision{}})
	assert.Empty(t, got)
}

func TestRoutingMarkerFor_SuggestionModeSuppressed(t *testing.T) {
	res := turnLoopResult{
		Decision:       router.Decision{Model: "gpt-5.5", Provider: "openai"},
		SuggestionMode: true,
		PlannerDecision: planner.Decision{
			Reason: planner.ReasonSameModel,
		},
	}
	got := routingMarkerFor(res)
	assert.Empty(t, got, "suggestion-mode responses must not emit the routing badge")
}

func TestRoutingMarkerFor_DropsProviderEvenWhenSet(t *testing.T) {
	got := routingMarkerFor(turnLoopResult{
		Decision: router.Decision{Model: "claude-haiku-4-5", Provider: "anthropic"},
		PlannerDecision: planner.Decision{
			Reason: planner.ReasonNoPin,
		},
	})
	assert.Contains(t, got, "✦ **Weave Router** → claude-haiku-4-5 ·")
	assert.NotContains(t, got, "(anthropic)", "provider must not leak into the user-facing marker")
	assert.NotContains(t, got, "()")
	assert.Contains(t, got, "· "+markerReasonBestPick)
}

func TestHumanReasonFromPlanner_UnknownCodeIsSilenced(t *testing.T) {
	// Unknown codes return empty so a new planner reason can't leak its
	// snake_case label into the user-facing marker.
	got := humanReasonFromPlanner("brand_new_reason_v9")
	assert.Empty(t, got)
}
