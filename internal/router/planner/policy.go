// Package planner decides, per turn, whether to stay on a session's
// pinned model (preserving the upstream prompt cache) or switch to the
// scorer's fresh recommendation. The verdict is a pure function of the
// pin row, the fresh decision, an estimated input-token count, and the
// currently-available model set; no I/O happens here.
//
// The EV math compares:
//
//	expected savings = (pin $/M-tok × pinMult − fresh $/M-tok × freshMult) × tokens × remaining-turn horizon
//	eviction cost    = fresh $/M-tok × tokens × (1 − freshMult)
//
// where pinMult / freshMult are each model's per-provider cache-read
// multiplier (Anthropic 0.10, OpenAI 0.50, Gemini 0.25, ...) read via
// pricing.Pricing.EffectiveCacheReadMultiplier. The planner switches
// when (expected savings - eviction cost) exceeds a configurable
// threshold OR when the tier-upgrade guard fires (fresh is in a
// strictly higher capability tier than pin; see
// internal/router/capability). The guard exists because a trivial
// first turn can pin a Low model and the pure EV math will then keep
// every harder prompt on the cheap pin. The cache-read multiplier on the savings term reflects
// that once a pin is warm, ~(1 - mult) of input tokens come from cache
// on both the pinned and (post-eviction) fresh model, so only the
// cache-read portion of the per-turn delta accrues over the horizon.
// Per-model multipliers (vs a single global) keep cross-provider
// switches (e.g. opus → gpt-5) economically correct. The full spec
// lives in the Prism-style cache-aware routing plan; this file is the
// executable form.
package planner

import (
	"workweave/router/internal/router"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/pricing"
	"workweave/router/internal/router/sessionpin"
)

// Outcome is the planner's verdict for this turn.
type Outcome int

const (
	// OutcomeStay keeps the request on the session's pinned model so the
	// upstream prompt cache stays warm.
	OutcomeStay Outcome = iota
	// OutcomeSwitch routes the turn to the fresh scorer recommendation,
	// accepting the one-time cache eviction cost.
	OutcomeSwitch
)

// Decision is the planner's output. Reason is a short snake_case label
// suitable for log/OTel attributes. The three USD fields are populated
// whenever the EV math ran (zero values are fine when it did not) so
// the orchestrator can stamp them as span attributes uniformly.
type Decision struct {
	Outcome            Outcome
	Reason             string
	ExpectedSavingsUSD float64
	EvictionCostUSD    float64
	ThresholdUSD       float64
}

// EVConfig parameterizes the policy. Constructed once at boot from env.
type EVConfig struct {
	// ThresholdUSD is the minimum positive EV (over the remaining-turn
	// horizon) required to switch off the pinned model. Default $0.001
	// keeps noise from flipping decisions while letting any non-trivial
	// arbitrage trigger a switch.
	ThresholdUSD float64
	// ExpectedRemainingTurns amortizes per-turn savings into a horizon.
	// Default 3 reflects observed session length distributions; tuned
	// per deployment via env.
	ExpectedRemainingTurns int
	// TierUpgradeEnabled overturns an EV "stay" when fresh is strictly
	// higher tier than pin (see internal/router/capability). Upgrade-
	// only by design — downgrades and sideways moves stay under the EV
	// verdict, which already routes cheap-fresh to switch.
	TierUpgradeEnabled bool
}

// Inputs is the full per-turn input to Decide. Kept as a struct (not
// positional args) because the call site grows over time.
type Inputs struct {
	Pin                  sessionpin.Pin
	Fresh                router.Decision
	EstimatedInputTokens int
	AvailableModels      map[string]struct{}
}

// Reason constants for Decision.Reason. Kept as exported strings so the
// orchestrator and tests reference the same labels without retyping.
const (
	ReasonNoPin           = "no_pin"
	ReasonSameModel       = "same_model"
	ReasonPinModelMissing = "pin_model_missing"
	ReasonNoPriorUsage    = "no_prior_usage"
	ReasonPricingMissing  = "pricing_missing"
	ReasonEVPositive      = "ev_positive"
	ReasonEVNegative      = "ev_negative"
	ReasonTierUpgrade     = "tier_upgrade"
)

// Decide returns the planner verdict for this turn. See package doc for
// the rule the implementation follows.
func Decide(in Inputs, cfg EVConfig) Decision {
	// No active pin → nothing to preserve, take the fresh recommendation.
	if in.Pin.Model == "" {
		return Decision{Outcome: OutcomeSwitch, Reason: ReasonNoPin}
	}

	// Fresh recommendation already matches the pin: trivial stay.
	if in.Fresh.Model == in.Pin.Model {
		return Decision{Outcome: OutcomeStay, Reason: ReasonSameModel}
	}

	// Pin's model is no longer routable (provider key removed, model
	// retired): we have to switch regardless of EV. nil AvailableModels
	// means "no filter configured" — preserve the pin in that case.
	if in.AvailableModels != nil {
		if _, ok := in.AvailableModels[in.Pin.Model]; !ok {
			return Decision{Outcome: OutcomeSwitch, Reason: ReasonPinModelMissing}
		}
	}

	// We have a pin but it has never completed a turn, so we have no
	// evidence the upstream cache is warm. Stay conservatively rather
	// than paying an eviction cost against a cold cache.
	if in.Pin.LastTurnEndedAt.IsZero() {
		return Decision{Outcome: OutcomeStay, Reason: ReasonNoPriorUsage}
	}

	pinPrice, ok1 := pricing.For(in.Pin.Model)
	freshPrice, ok2 := pricing.For(in.Fresh.Model)
	if !ok1 || !ok2 {
		return Decision{Outcome: OutcomeStay, Reason: ReasonPricingMissing}
	}

	tokens := float64(in.EstimatedInputTokens)
	// Per-model cache-read multipliers scale the per-turn delta because,
	// in steady state on either model, ~(1 - multiplier) of input tokens
	// are served from the prompt cache. Switching only avoids cost on the
	// cache-read portion of subsequent turns; pricing the savings off full
	// input price systematically overstates the switch benefit. Reading
	// the multiplier per-model (vs a single global) is what makes
	// cross-provider switches (e.g. opus → gpt-5, where Anthropic's 0.10
	// and OpenAI's 0.50 differ by 5×) economically correct.
	pinMult := pinPrice.EffectiveCacheReadMultiplier()
	freshMult := freshPrice.EffectiveCacheReadMultiplier()
	savingsPerTurn := (pinPrice.InputUSDPer1M*pinMult - freshPrice.InputUSDPer1M*freshMult) * tokens / 1e6
	evictionCost := freshPrice.InputUSDPer1M * tokens * (1 - freshMult) / 1e6
	expectedSavings := savingsPerTurn * float64(cfg.ExpectedRemainingTurns)

	d := Decision{
		ExpectedSavingsUSD: expectedSavings,
		EvictionCostUSD:    evictionCost,
		ThresholdUSD:       cfg.ThresholdUSD,
	}
	switch {
	case expectedSavings-evictionCost > cfg.ThresholdUSD:
		d.Outcome = OutcomeSwitch
		d.Reason = ReasonEVPositive
	case cfg.TierUpgradeEnabled && tierUpgrade(in.Pin.Model, in.Fresh.Model):
		// EV said stay; pay the eviction cost for a stronger model.
		d.Outcome = OutcomeSwitch
		d.Reason = ReasonTierUpgrade
	default:
		d.Outcome = OutcomeStay
		d.Reason = ReasonEVNegative
	}
	return d
}

// tierUpgrade reports whether fresh is strictly higher tier than pin.
// Unknown on either side disables the guard so a missing entry never
// silently forces a switch.
func tierUpgrade(pin, fresh string) bool {
	pinTier := capability.TierFor(pin)
	freshTier := capability.TierFor(fresh)
	if pinTier == capability.TierUnknown || freshTier == capability.TierUnknown {
		return false
	}
	return freshTier > pinTier
}
