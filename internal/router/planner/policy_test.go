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
	modelGPT5    = "gpt-5"             // $2.50 input / $10.00 output, cache mult 0.10 (cross-provider)
	modelUnknown = "fictional-foo-1.0" // intentionally absent from the pricing table
)

// defaultCfg mirrors production defaults (threshold $0.001, horizon 3 turns).
var defaultCfg = planner.EVConfig{
	ThresholdUSD:           0.001,
	ExpectedRemainingTurns: 3,
}

// availableAll covers every model the EV cases reference; used everywhere
// except pin_model_missing.
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

// A session pinned to a cheap model must switch to a subscription-covered model
// once the subsidy makes it near-free — otherwise the discount never takes
// effect on sticky sessions.
func TestDecide_SubscriptionDiscountFlipsSwitch(t *testing.T) {
	t.Parallel()
	base := planner.Inputs{
		Pin:                  pinWithUsage(modelHaiku),
		Fresh:                router.Decision{Model: modelOpus},
		EstimatedInputTokens: 100_000,
		AvailableModels:      availableAll,
	}

	stay := planner.Decide(base, defaultCfg)
	assert.Equal(t, planner.OutcomeStay, stay.Outcome, "no subsidy: keep the cheap pin")

	// Subsidize the fresh model to ~free -> switching now saves.
	sub := base
	sub.SubsidizedCostFactor = map[string]float64{modelOpus: 0.01}
	switched := planner.Decide(sub, defaultCfg)
	assert.Equal(t, planner.OutcomeSwitch, switched.Outcome,
		"subsidized covered model must win the stay-vs-switch EV")
	assert.Equal(t, planner.ReasonEVPositive, switched.Reason)

	// A 0.0 factor is still "covered" (map membership decides, not sign).
	zeroFactor := base
	zeroFactor.SubsidizedCostFactor = map[string]float64{modelOpus: 0.0}
	zero := planner.Decide(zeroFactor, defaultCfg)
	assert.Equal(t, planner.OutcomeSwitch, zero.Outcome,
		"a 0.0 covered-model factor must still be treated as free (switch), not uncovered")
}

// With ColdPinFollowFresh enabled, a cold pin follows the scorer's fresh pick
// even when the raw-price EV is below threshold.
func TestDecide_ColdPinFollowFresh(t *testing.T) {
	t.Parallel()
	coldCfg := planner.EVConfig{
		ThresholdUSD:           0.001,
		ExpectedRemainingTurns: 3,
		ColdPinFollowFresh:     true,
	}
	// Cheap pin → expensive fresh: raw-price EV is strongly negative, so only
	// the cold-pin lever can flip it.
	base := planner.Inputs{
		Pin:                  pinWithUsage(modelHaiku),
		Fresh:                router.Decision{Model: modelOpus},
		EstimatedInputTokens: 50_000,
		AvailableModels:      availableAll,
		PinCacheCold:         true,
	}

	got := planner.Decide(base, coldCfg)
	assert.Equal(t, planner.OutcomeSwitch, got.Outcome, "cold pin + lever on must follow the fresh pick")
	assert.Equal(t, planner.ReasonColdPinFresh, got.Reason)
	assert.True(t, got.PinCacheCold, "decision must echo the cold pricing assumption")

	off := planner.Decide(base, defaultCfg)
	assert.Equal(t, planner.OutcomeStay, off.Outcome, "lever off must preserve the EV-negative stay")
	assert.Equal(t, planner.ReasonEVNegative, off.Reason)

	warm := base
	warm.PinCacheCold = false
	stay := planner.Decide(warm, coldCfg)
	assert.Equal(t, planner.OutcomeStay, stay.Outcome, "warm pin must not follow fresh on the cold lever")
	assert.Equal(t, planner.ReasonEVNegative, stay.Reason)

	// A cold pin whose switch is already EV-positive keeps the more specific
	// ev_positive reason.
	evPositive := planner.Inputs{
		Pin:                  pinWithUsage(modelOpus),
		Fresh:                router.Decision{Model: modelHaiku},
		EstimatedInputTokens: 50_000,
		AvailableModels:      availableAll,
		PinCacheCold:         true,
	}
	pos := planner.Decide(evPositive, coldCfg)
	assert.Equal(t, planner.OutcomeSwitch, pos.Outcome)
	assert.Equal(t, planner.ReasonEVPositive, pos.Reason, "EV-positive must take precedence over the cold-pin reason")
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
			// opus -> haiku, 50k tokens, 3 turns; cache-read multiplier 0.1 applies.
			//   savingsPerTurn=$0.021 evictionCost=$0.036
			//   expectedSavings=$0.063 -> delta=$0.027 -> Switch.
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
			// Symmetric flip: haiku pin, opus fresh.
			//   expectedSavings=-$0.063 evictionCost=$0.225 -> delta=-$0.288 -> Stay.
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
			// opus -> haiku, tuned to land just below threshold (net $0.00099954
			// at 1851 tokens vs $0.001 threshold, 0.05% below) -> Stay.
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
			// Same math, two extra tokens (1853) nudges net to $0.00100062,
			// 0.06% above threshold -> Switch.
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
			// Cross-provider: opus -> gpt-5, 50k prompt. gpt-5 is cheaper
			// per-token in cache steady-state (expectedSavings=$0.0375), but
			// evicting opus's warm cache to refill gpt-5's cold one costs more
			// (evictionCost=$0.1125) -> Stay.
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
			wantEvictionCostUSD:    0.1125,
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
			// Same EV-loss as above; tier guard flips it since opus outranks haiku.
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
			// expectedSavings=$0.00066 evictionCost=$0.00072
			wantExpectedSavingsUSD: 0.00066,
			wantEvictionCostUSD:    0.00072,
		},
		{
			// Cold pin: cache TTL lapsed, both sides price uncached, so this
			// switches on raw input price rather than the cache-read delta.
			// expectedSavings=$0.63 evictionCost=$0 (nothing warm to evict).
			name: "cold_ev_positive: opus -> haiku prices uncached",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelHaiku},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
				PinCacheCold:         true,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.63,
			wantEvictionCostUSD:    0,
		},
		{
			// Cold twin of ev_cross_provider (which STAYS): with no warm cache
			// to preserve, raw $2.50 vs $5.00 input price wins and it switches.
			name: "cold_cross_provider: opus -> gpt-5 switches once cache is cold",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelOpus),
				Fresh:                router.Decision{Model: modelGPT5},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
				PinCacheCold:         true,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonEVPositive},
			expectEVMath:           true,
			wantExpectedSavingsUSD: 0.375,
			wantEvictionCostUSD:    0,
		},
		{
			// Cold pin on the cheaper model: no cache to preserve, but switching
			// to the pricier fresh model is still a raw-price loss -> Stay.
			name: "cold_ev_negative: haiku -> opus stays on raw price",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelHaiku),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
				PinCacheCold:         true,
			},
			cfg:                    defaultCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeStay, Reason: planner.ReasonEVNegative},
			expectEVMath:           true,
			wantExpectedSavingsUSD: -0.63,
			wantEvictionCostUSD:    0,
		},
		{
			// Cold pin does not disable the tier-upgrade guard: EV alone would
			// stay, but opus outranks haiku so it still switches.
			name: "cold_tier_upgrade: guard still fires when cache is cold",
			in: planner.Inputs{
				Pin:                  pinWithUsage(modelHaiku),
				Fresh:                router.Decision{Model: modelOpus},
				EstimatedInputTokens: 50_000,
				AvailableModels:      availableAll,
				PinCacheCold:         true,
			},
			cfg:                    tierUpgradeCfg,
			want:                   planner.Decision{Outcome: planner.OutcomeSwitch, Reason: planner.ReasonTierUpgrade},
			expectEVMath:           true,
			wantExpectedSavingsUSD: -0.63,
			wantEvictionCostUSD:    0,
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
				assert.Equal(t, tc.in.PinCacheCold, got.PinCacheCold, "pin_cache_cold echoed")
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
