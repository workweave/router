package planner_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"workweave/router/internal/router"
	"workweave/router/internal/router/planner"
	"workweave/router/internal/router/sessionpin"
)

const (
	modelOpus    = "claude-opus-4-7"   // $15.00 input / $75.00 output per 1M
	modelSonnet  = "claude-sonnet-4-5" // $3.00 input / $15.00 output
	modelHaiku   = "claude-haiku-4-5"  // $0.80 input / $4.00 output
	modelUnknown = "fictional-foo-1.0" // intentionally absent from the pricing table
)

// defaultCfg matches the documented planner defaults (threshold $0.001,
// horizon 3 turns) so the table below mirrors what production sees.
var defaultCfg = planner.EVConfig{
	ThresholdUSD:           0.001,
	ExpectedRemainingTurns: 3,
}

// availableAll covers the three Anthropic models the EV cases reference.
// Used everywhere except the pin_model_missing case.
var availableAll = map[string]struct{}{
	modelOpus:   {},
	modelSonnet: {},
	modelHaiku:  {},
}

// pinWithUsage returns a populated pin that has completed at least one
// turn, so the planner's LastTurnEndedAt-zero guard does not fire.
func pinWithUsage(model string) sessionpin.Pin {
	return sessionpin.Pin{
		Model:           model,
		LastTurnEndedAt: time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC),
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   planner.Inputs
		cfg  planner.EVConfig
		want planner.Decision
		// expectEVMath asserts that ExpectedSavingsUSD / EvictionCostUSD
		// are populated against the hand-computed expectations.
		expectEVMath           bool
		wantExpectedSavingsUSD float64
		wantEvictionCostUSD    float64
	}{
		{
			name: "no_pin: zero-value pin always switches",
			in: planner.Inputs{
				Pin:                  sessionpin.Pin{},
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 1000,
				AvailableModels:      availableAll,
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonNoPin},
		},
		{
			name: "same_model: fresh recommendation matches the pin",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonSameModel},
		},
		{
			name: "pin_model_missing: pin model not in availability set",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 50_000,
				// modelOpus deliberately absent — provider key was removed.
				AvailableModels: map[string]struct{}{modelHaiku: {}, modelSonnet: {}},
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonPinModelMissing},
		},
		{
			name: "no_prior_usage: pin populated but LastTurnEndedAt zero",
			in: planner.Inputs{
				// Model set but no completed turn → conservative stay.
				Pin:                  sessionpin.Pin{Model: modelOpus},
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonNoPriorUsage},
		},
		{
			name: "pricing_missing: pin model unknown to price table",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelUnknown),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 50_000,
				AvailableModels:      map[string]struct{}{modelUnknown: {}, modelHaiku: {}},
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonPricingMissing},
		},
		{
			name: "pricing_missing: fresh model unknown to price table",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelUnknown},
				EstimatedInputTokens: 50_000,
				AvailableModels:      map[string]struct{}{modelOpus: {}, modelUnknown: {}},
			},
			cfg:  defaultCfg,
			want: planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonPricingMissing},
		},
		{
			// EV math for switch from opus -> haiku, 50k input, 3 turns:
			//   savingsPerTurn = (15.00 - 0.80) * 50000 / 1e6 = 14.20 * 0.05 = $0.71
			//   evictionCost   = 0.80 * 50000 * 0.9 / 1e6     = 0.80 * 0.045 = $0.036
			//   expectedSavings = 0.71 * 3 = $2.13
			//   delta = 2.13 - 0.036 = $2.094 >> $0.001 -> Switch.
			name: "ev_positive: opus -> haiku on a large prompt",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 2.13,
			wantEvictionCostUSD:    0.036,
		},
		{
			// Symmetric flip: pinning haiku, fresh recommends opus.
			//   savingsPerTurn = (0.80 - 15.00) * 0.05 = -$0.71
			//   evictionCost   = 15.00 * 0.045         = $0.675
			//   expectedSavings = -0.71 * 3 = -$2.13
			//   delta = -2.13 - 0.675 = -$2.805 < $0.001 -> Stay.
			name: "ev_negative: haiku -> opus is a huge net loss",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelHaiku),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: -2.13,
			wantEvictionCostUSD:    0.675,
		},
		{
			// Sonnet -> haiku, tuned to land just BELOW threshold.
			//   per-token net = (2.20 * 3 - 0.72) / 1e6 = 5.88 / 1e6
			//   at tokens = 170: net = 5.88 * 170 / 1e6 = $0.0009996
			//   threshold = $0.001 -> net is 0.04% below; within ±5%.
			//   expectedSavings = 2.20 * 170 / 1e6 * 3 = $0.001122
			//   evictionCost    = 0.80 * 170 * 0.9 / 1e6 = $0.0001224
			//   delta = 0.001122 - 0.0001224 = 0.0009996 -> Stay.
			name: "ev_near_threshold: just below threshold stays stable",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelSonnet),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 170,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.001122,
			wantEvictionCostUSD:    0.0001224,
		},
		{
			// Same sonnet -> haiku math, one extra token nudges across:
			//   at tokens = 171: net = 5.88 * 171 / 1e6 = $0.00100548
			//   threshold = $0.001 -> net is 0.5% above; within ±5%.
			//   expectedSavings = 2.20 * 171 / 1e6 * 3 = $0.0011286
			//   evictionCost    = 0.80 * 171 * 0.9 / 1e6 = $0.00012312
			//   delta = 0.0011286 - 0.00012312 = 0.00100548 -> Switch.
			name: "ev_near_threshold: just above threshold flips to switch",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelSonnet),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 171,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.0011286,
			wantEvictionCostUSD:    0.00012312,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := planner.Decide(tc.in, tc.cfg)
			assert.Equal(t, tc.want.Outcome, got.Outcome, "outcome")
			assert.Equal(t, tc.want.Reason, got.Reason, "reason")

			if tc.expectEVMath {
				// The math is exact-rational on these inputs so a tight
				// tolerance is fine; we only allow float64 rounding noise.
				assert.InDelta(t, tc.wantExpectedSavingsUSD, got.ExpectedSavingsUSD, 1e-9, "expected_savings_usd")
				assert.InDelta(t, tc.wantEvictionCostUSD, got.EvictionCostUSD, 1e-9, "eviction_cost_usd")
				assert.Equal(t, tc.cfg.ThresholdUSD, got.ThresholdUSD, "threshold_usd echoed")
			} else {
				// When the EV math never ran, all three USD fields are
				// left zero — this is what the orchestrator stamps for
				// non-EV reasons.
				assert.Zero(t, got.ExpectedSavingsUSD, "expected_savings_usd zero when EV unused")
				assert.Zero(t, got.EvictionCostUSD, "eviction_cost_usd zero when EV unused")
				assert.Zero(t, got.ThresholdUSD, "threshold_usd zero when EV unused")
			}
		})
	}
}
