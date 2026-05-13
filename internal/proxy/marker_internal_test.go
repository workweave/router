package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/translate"
)

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
			name: "no planner ran, fresh decision: first-turn fallback reason",
			res: turnLoopResult{
				Decision: decision,
			},
			wantContains: []string{
				"reason: first turn",
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
	assert.Contains(t, got, "reason: first turn")
}

func TestHumanReasonFromPlanner_UnknownCodePassesThrough(t *testing.T) {
	got := humanReasonFromPlanner("brand_new_reason_v9")
	assert.Equal(t, "brand_new_reason_v9", got)
}

func TestClosingMarkerFor_EmitsSavingsWhenRoutedIsCheaper(t *testing.T) {
	// opus 4.7 ($15/$75) vs deepseek-v4-pro ($0.435/$0.870), 0.10 cache mult,
	// 10k non-cached + 5k cache-read in / 1k out → savings = $0.2271.
	fn := closingMarkerFor(
		router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "openrouter"},
		"claude-opus-4-7",
	)
	got := fn(translate.Usage{InputTokens: 15000, OutputTokens: 1000, CacheReadTokens: 5000})
	assert.Equal(t, "✦ saved $0.2271 vs claude-opus-4-7 (15k in / 1k out)", got)
}

func TestClosingMarkerFor_FormatsTokenCounts(t *testing.T) {
	fn := closingMarkerFor(
		router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "openrouter"},
		"claude-opus-4-7",
	)
	cases := []struct {
		name string
		u    translate.Usage
		want string
	}{
		{
			name: "thousands input, sub-thousand output",
			u:    translate.Usage{InputTokens: 127053, OutputTokens: 50},
			want: "(127k in / 50 out)",
		},
		{
			name: "sub-thousand both",
			u:    translate.Usage{InputTokens: 850, OutputTokens: 42},
			want: "(850 in / 42 out)",
		},
		{
			name: "million-plus input",
			u:    translate.Usage{InputTokens: 1_500_000, OutputTokens: 2000},
			want: "(1.5M in / 2k out)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fn(tc.u)
			assert.Contains(t, got, tc.want)
		})
	}
}

func TestClosingMarkerFor_NoEmissionWhenRoutedEqualsRequested(t *testing.T) {
	fn := closingMarkerFor(
		router.Decision{Model: "claude-opus-4-7", Provider: "anthropic"},
		"claude-opus-4-7",
	)
	got := fn(translate.Usage{InputTokens: 10000, OutputTokens: 500})
	assert.Empty(t, got)
}

func TestClosingMarkerFor_NoEmissionWhenSavingsAreNonPositive(t *testing.T) {
	// Routed to the more expensive side — marker mustn't advertise a loss.
	fn := closingMarkerFor(
		router.Decision{Model: "claude-opus-4-7", Provider: "anthropic"},
		"deepseek/deepseek-v4-pro",
	)
	got := fn(translate.Usage{InputTokens: 15000, OutputTokens: 1000, CacheReadTokens: 5000})
	assert.Empty(t, got)
}

func TestClosingMarkerFor_NoEmissionWhenPricingMissing(t *testing.T) {
	fn := closingMarkerFor(
		router.Decision{Model: "imaginary-model-v0", Provider: "openrouter"},
		"claude-opus-4-7",
	)
	got := fn(translate.Usage{InputTokens: 10000, OutputTokens: 1000})
	assert.Empty(t, got)
}

func TestClosingMarkerFor_NoEmissionWhenSavingsBelowEpsilon(t *testing.T) {
	// Savings below $0.0001 → skip so the marker doesn't flicker.
	fn := closingMarkerFor(
		router.Decision{Model: "deepseek/deepseek-v4-pro", Provider: "openrouter"},
		"claude-opus-4-7",
	)
	got := fn(translate.Usage{InputTokens: 1, OutputTokens: 1})
	assert.Empty(t, got)
}
