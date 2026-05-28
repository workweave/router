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
	modelOpus    = "claude-opus-4-7"   // $5.00 input / $25.00 output per 1M, cache mult 0.10
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
			//   savingsPerTurn = (5.00 - 0.80) * 0.1 * 50000 / 1e6 = $0.021
			//   evictionCost   = 0.80 * 50000 * 0.9 / 1e6          = $0.036
			//   expectedSavings = 0.021 * 3 = $0.063
			//   delta = 0.063 - 0.036 = $0.027 > $0.001 -> Switch.
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
			wantExpectedSavingsUSD: 0.063,
			wantEvictionCostUSD:    0.036,
		},
		{
			// Symmetric flip: pinning haiku, fresh recommends opus.
			//   savingsPerTurn = (0.80 - 5.00) * 0.1 * 50000 / 1e6 = -$0.021
			//   evictionCost   = 5.00 * 50000 * 0.9 / 1e6          = $0.225
			//   expectedSavings = -0.021 * 3 = -$0.063
			//   delta = -0.063 - 0.225 = -$0.288 < $0.001 -> Stay.
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
			wantExpectedSavingsUSD: -0.063,
			wantEvictionCostUSD:    0.225,
		},
		{
			// Opus -> haiku, tuned to land just BELOW threshold under the
			// corrected (cache-read-aware) EV math at the new $5/$25 opus
			// pricing (was $15/$75; the old opus->sonnet boundary case no
			// longer straddles — sonnet's $3 input dominates opus's $0.50
			// cache-read at every horizon).
			//   per-token net = (3 * 0.1 * (5.00 - 0.80) - 0.9 * 0.80) / 1e6
			//                 = (1.26 - 0.72) / 1e6 = 0.54 / 1e6
			//   at tokens = 1851: net = 0.54 * 1851 / 1e6 = $0.00099954
			//   threshold = $0.001 -> net is 0.05% below.
			//   expectedSavings = (5.00 - 0.80) * 0.1 * 1851 / 1e6 * 3 = $0.00233226
			//   evictionCost    = 0.80 * 1851 * 0.9 / 1e6              = $0.00133272
			//   delta = 0.00233226 - 0.00133272 = $0.00099954 -> Stay.
			name: "ev_near_threshold: just below threshold stays stable",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 1851,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.00233226,
			wantEvictionCostUSD:    0.00133272,
		},
		{
			// Same opus -> haiku math, two extra tokens nudge across:
			//   at tokens = 1853: net = 0.54 * 1853 / 1e6 = $0.00100062
			//   threshold = $0.001 -> net is 0.06% above.
			//   expectedSavings = (5.00 - 0.80) * 0.1 * 1853 / 1e6 * 3 = $0.00233478
			//   evictionCost    = 0.80 * 1853 * 0.9 / 1e6              = $0.00133416
			//   delta = 0.00233478 - 0.00133416 = $0.00100062 -> Switch.
			name: "ev_near_threshold: just above threshold flips to switch",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 1853,
				AvailableModels:      availableAll,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.00233478,
			wantEvictionCostUSD:    0.00133416,
		},
		{
			// Cross-provider regression: opus (Anthropic, mult 0.10) ->
			// gpt-5 (OpenAI, mult 0.50) on a 50k prompt at the new $5/$25
			// opus pricing.
			//   savingsPerTurn  = (5.00 * 0.10 - 2.50 * 0.50) * 50000 / 1e6
			//                   = (0.50 - 1.25) * 0.05 = -$0.0375
			//   expectedSavings = -0.0375 * 3 = -$0.1125
			//   evictionCost    = 2.50 * 50000 * (1 - 0.50) / 1e6 = $0.0625
			//   delta = -0.1125 - 0.0625 = -$0.175 -> Stay.
			//
			// Under the (now-corrected) catalog pricing, opus's per-token
			// cost matches gpt-5's nominal input price, but gpt-5's 50%
			// cache-read multiplier (vs opus's 10%) actually makes gpt-5
			// *more* expensive in cache steady-state — so the planner
			// stays on opus regardless of any prompt size. The old global-
			// 0.10 path would have wrongly switched here.
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
			wantExpectedSavingsUSD: -0.1125,
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
			wantExpectedSavingsUSD: -0.063,
			wantEvictionCostUSD:    0.225,
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
			wantExpectedSavingsUSD: -0.063,
			wantEvictionCostUSD:    0.225,
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
