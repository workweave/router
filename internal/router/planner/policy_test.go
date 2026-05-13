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
	modelOpus    = "claude-opus-4-7"   // $15.00 input / $75.00 output per 1M, cache mult 0.10
	modelSonnet  = "claude-sonnet-4-5" // $3.00 input / $15.00 output, cache mult 0.10
	modelHaiku   = "claude-haiku-4-5"  // $0.80 input / $4.00 output, cache mult 0.10
	modelGPT5    = "gpt-5"             // $2.50 input / $10.00 output, cache mult 0.50 (cross-provider)
	modelUnknown = "fictional-foo-1.0" // intentionally absent from the pricing table
)

// defaultCfg matches the documented planner defaults (threshold $0.001,
// horizon 3 turns) so the table below mirrors what production sees.
var defaultCfg = planner.EVConfig{
	ThresholdUSD:           0.001,
	ExpectedRemainingTurns: 3,
}

// availableAll covers every model the EV cases reference (Anthropic + the
// cross-provider GPT-5 entry). Used everywhere except the pin_model_missing
// case.
var availableAll = map[string]struct{}{
	modelOpus:   {},
	modelSonnet: {},
	modelHaiku:  {},
	modelGPT5:   {},
}

// tierUpgradeCfg mirrors defaultCfg with the tier guard on.
var tierUpgradeCfg = planner.EVConfig{
	ThresholdUSD:           0.001,
	ExpectedRemainingTurns: 3,
	TierUpgradeEnabled:     true,
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
			// EV math for switch from opus -> haiku, 50k input, 3 turns.
			// Savings are multiplied by CacheReadMultiplier (0.1) because in
			// steady state most input tokens come from cache on both models:
			//   savingsPerTurn = (15.00 - 0.80) * 0.1 * 50000 / 1e6 = $0.071
			//   evictionCost   = 0.80 * 50000 * 0.9 / 1e6           = $0.036
			//   expectedSavings = 0.071 * 3 = $0.213
			//   delta = 0.213 - 0.036 = $0.177 >> $0.001 -> Switch.
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
			wantExpectedSavingsUSD: 0.213,
			wantEvictionCostUSD:    0.036,
		},
		{
			// Symmetric flip: pinning haiku, fresh recommends opus.
			//   savingsPerTurn = (0.80 - 15.00) * 0.1 * 50000 / 1e6 = -$0.071
			//   evictionCost   = 15.00 * 50000 * 0.9 / 1e6          = $0.675
			//   expectedSavings = -0.071 * 3 = -$0.213
			//   delta = -0.213 - 0.675 = -$0.888 < $0.001 -> Stay.
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
			wantExpectedSavingsUSD: -0.213,
			wantEvictionCostUSD:    0.675,
		},
		{
			// Opus -> sonnet, tuned to land just BELOW threshold under the
			// corrected (cache-read-aware) EV math.
			//   per-token net = (3 * 0.1 * (15.00 - 3.00) - 0.9 * 3.00) / 1e6
			//                 = (3.60 - 2.70) / 1e6 = 0.90 / 1e6
			//   at tokens = 1111: net = 0.90 * 1111 / 1e6 = $0.0009999
			//   threshold = $0.001 -> net is 0.01% below.
			//   expectedSavings = (15.00 - 3.00) * 0.1 * 1111 / 1e6 * 3 = $0.0039996
			//   evictionCost    = 3.00 * 1111 * 0.9 / 1e6              = $0.0029997
			//   delta = 0.0039996 - 0.0029997 = $0.0009999 -> Stay.
			// (sonnet -> haiku no longer straddles threshold: under the
			// corrected math its per-token net is negative at every horizon.)
			name: "ev_near_threshold: just below threshold stays stable",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelSonnet},
				EstimatedInputTokens: 1111,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.0039996,
			wantEvictionCostUSD:    0.0029997,
		},
		{
			// Same opus -> sonnet math, one extra token nudges across:
			//   at tokens = 1112: net = 0.90 * 1112 / 1e6 = $0.0010008
			//   threshold = $0.001 -> net is 0.08% above.
			//   expectedSavings = (15.00 - 3.00) * 0.1 * 1112 / 1e6 * 3 = $0.0040032
			//   evictionCost    = 3.00 * 1112 * 0.9 / 1e6              = $0.0030024
			//   delta = 0.0040032 - 0.0030024 = $0.0010008 -> Switch.
			name: "ev_near_threshold: just above threshold flips to switch",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelSonnet},
				EstimatedInputTokens: 1112,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.0040032,
			wantEvictionCostUSD:    0.0030024,
		},
		{
			// Cross-provider regression: opus (Anthropic, mult 0.10) ->
			// gpt-5 (OpenAI, mult 0.50) on a 50k prompt.
			//   savingsPerTurn  = (15.00 * 0.10 - 2.50 * 0.50) * 50000 / 1e6
			//                   = (1.50 - 1.25) * 0.05 = $0.0125
			//   expectedSavings = 0.0125 * 3 = $0.0375
			//   evictionCost    = 2.50 * 50000 * (1 - 0.50) / 1e6 = $0.0625
			//   delta = 0.0375 - 0.0625 = -$0.025 -> Stay.
			//
			// Sanity vs the old (broken) global-0.10 math: that path would
			// have computed savingsPerTurn = (15 - 2.5) * 0.10 * 0.05 =
			// $0.0625 and evictionCost = 2.50 * 0.05 * 0.90 = $0.1125, delta
			// $0.075 — i.e. would have wrongly chosen to switch. Per-model
			// multipliers correctly say "stay on opus" because gpt-5's
			// 50% cache-read pricing eats most of the per-turn savings.
			name: "ev_cross_provider: opus -> gpt-5 stays under per-model math",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelGPT5},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.0375,
			wantEvictionCostUSD:    0.0625,
		},
		{
			// Mirror of ev_negative; pinned here so the next case can show
			// the tier knob is what flips the verdict.
			name: "tier_upgrade_disabled: low -> high still stays",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelHaiku),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg, // TierUpgradeEnabled = false
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: -0.213,
			wantEvictionCostUSD:    0.675,
		},
		{
			// Same EV-loss as above; guard on flips it because opus is
			// strictly higher tier than haiku. USD fields still populated.
			name: "tier_upgrade: low -> high flips stay into switch",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelHaiku),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
			},
			cfg:                    tierUpgradeCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonTierUpgrade},
			expectEVMath:           true,
			wantExpectedSavingsUSD: -0.213,
			wantEvictionCostUSD:    0.675,
		},
		{
			// Sonnet (Mid) -> haiku (Low) is a downgrade; guard must
			// not fire, EV math governs.
			name: "tier_upgrade: downgrade does not trigger guard",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelSonnet),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 1000,
				AvailableModels:      availableAll,
			},
			cfg:          tierUpgradeCfg,
			want:         planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath: true,
			// savingsPerTurn = (3.00 - 0.80) * 0.1 * 1000 / 1e6 = $0.00022
			// expectedSavings = 0.00022 * 3 = $0.00066
			// evictionCost   = 0.80 * 1000 * 0.9 / 1e6 = $0.00072
			wantExpectedSavingsUSD: 0.00066,
			wantEvictionCostUSD:    0.00072,
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
