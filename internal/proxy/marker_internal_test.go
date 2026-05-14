package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/planner"
)

func TestRoutingMarkerFor_TierClampNote(t *testing.T) {
	t.Parallel()

	t.Run("haiku-ceiling clamp suggests sonnet upsell", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-haiku-4-5", Provider: "anthropic"},
			TierClamped:   true,
			PreClampModel: "deepseek/deepseek-v4-pro",
			RequestedTier: capability.TierLow,
		}
		got := routingMarkerFor(res)
		assert.Contains(t, got, "second-choice pick")
		assert.Contains(t, got, "deepseek/deepseek-v4-pro")
		assert.Contains(t, got, "low tier")
		assert.Contains(t, got, "claude-sonnet-4-5")
	})

	t.Run("sonnet-ceiling clamp suggests opus upsell", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-sonnet-4-5", Provider: "anthropic"},
			TierClamped:   true,
			PreClampModel: "claude-opus-4-7",
			RequestedTier: capability.TierMid,
		}
		got := routingMarkerFor(res)
		assert.Contains(t, got, "claude-opus-4-7")
		assert.Contains(t, got, "mid tier")
		assert.Contains(t, got, "claude-opus-4-7 to unlock")
	})

	t.Run("no clamp emits no note", func(t *testing.T) {
		res := turnLoopResult{
			Decision:      router.Decision{Model: "claude-opus-4-7", Provider: "anthropic"},
			TierClamped:   false,
			RequestedTier: capability.TierHigh,
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
	}{
		{
			name: "ev_positive: planner switched",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonEVPositive},
			},
			wantContains: []string{
				"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter)",
				"reason: switched to save on cache reads",
			},
			wantNotContain: []string{
				"ev_positive",
				"est. save",
				"saved $",
			},
		},
		{
			name: "ev_negative: planner stayed",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonEVNegative},
			},
			wantContains: []string{
				"reason: stayed: cache reuse beats the switch",
			},
			wantNotContain: []string{
				"ev_negative",
				"est. save",
			},
		},
		{
			name: "same_model: planner ran with trivial outcome",
			res: turnLoopResult{
				Decision:        decision,
				PlannerDecision: planner.Decision{Reason: planner.ReasonSameModel},
			},
			wantContains: []string{
				"reason: scorer matches the pin",
			},
			wantNotContain: []string{
				"est. save",
				"same_model",
			},
		},
		{
			name: "no planner ran, fresh decision: top-scorer fallback reason",
			res: turnLoopResult{
				Decision: decision,
			},
			wantContains: []string{
				"reason: top scorer",
			},
			wantNotContain: []string{
				"est. save",
			},
		},
		{
			name: "hard-pin path: planner didn't run, mark it accordingly",
			res: turnLoopResult{
				Decision:   decision,
				HardPinned: true,
			},
			wantContains: []string{
				"reason: hard pin (compaction / sub-agent)",
			},
			wantNotContain: []string{
				"est. save",
			},
		},
		{
			name: "tool-result short-circuit: pinned model reused, no planner",
			res: turnLoopResult{
				Decision:  decision,
				StickyHit: true,
			},
			wantContains: []string{
				"reason: tool-result follow-up",
			},
			wantNotContain: []string{
				"est. save",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := routingMarkerFor(tc.res)
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

func TestRoutingMarkerFor_OmitsProviderParensWhenProviderMissing(t *testing.T) {
	got := routingMarkerFor(turnLoopResult{
		Decision: router.Decision{Model: "claude-haiku-4-5"},
		PlannerDecision: planner.Decision{
			Reason: planner.ReasonNoPin,
		},
	})
	assert.Contains(t, got, "✦ **Weave Router** → claude-haiku-4-5 ·")
	assert.NotContains(t, got, "()")
	assert.Contains(t, got, "reason: top scorer")
}

func TestHumanReasonFromPlanner_UnknownCodePassesThrough(t *testing.T) {
	got := humanReasonFromPlanner("brand_new_reason_v9")
	assert.Equal(t, "brand_new_reason_v9", got)
}

