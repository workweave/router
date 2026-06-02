// Package planner decides, per turn, whether to stay on a session's
// pinned model (preserving the upstream prompt cache) or switch to the
// scorer's fresh recommendation. Pure function of (pin, fresh decision,
// estimated tokens, available models); no I/O.
//
// EV math:
//
//	savings_per_turn = (pin $/M-tok × pinMult − fresh $/M-tok × freshMult) × tokens
//	eviction_cost    = fresh $/M-tok × tokens × (1 − freshMult)
//
// where pinMult/freshMult are per-model cache-read multipliers from
// catalog.Pricing.EffectiveCacheReadMultiplier. Switches when
// (expected_savings − eviction_cost) > threshold, or when tier-upgrade
// guard fires (fresh is strictly higher tier than pin).
//
// Cache-warmth gate: the cache-read multipliers and the eviction cost only
// apply while the pin's upstream prompt cache is still warm. When Inputs
// reports the pin cold (the provider's cache TTL has lapsed — short and
// best-effort on the OSS compat providers, unlike Anthropic's 1h window),
// both sides are priced uncached (pinMult = freshMult = 1, eviction_cost = 0):
// staying buys no cache reuse the fresh route wouldn't also pay, and the
// one-time prefill is incurred either way. This stops a phantom cache from
// gluing a session to a stale pin once the real cache is gone.
package planner

import (
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/sessionpin"
)

// Outcome is the planner's verdict for this turn.
type Outcome int

const (
	OutcomeStay   Outcome = iota // Keep on pinned model.
	OutcomeSwitch                // Route to fresh scorer recommendation.
)

// Decision is the planner's output. Reason is a short snake_case label.
type Decision struct {
	Outcome            Outcome
	Reason             string
	ExpectedSavingsUSD float64
	EvictionCostUSD    float64
	ThresholdUSD       float64
	// PinCacheCold echoes the warmth assumption the EV math ran under, for
	// observability. Only meaningful on the EV path; false on early returns.
	PinCacheCold bool
}

// EVConfig parameterizes the policy. Constructed once at boot from env.
type EVConfig struct {
	// ThresholdUSD is the minimum positive EV over the horizon to switch.
	// Default $0.001 keeps noise from flipping decisions.
	ThresholdUSD float64
	// ExpectedRemainingTurns amortizes per-turn savings into a horizon.
	// Default 3 reflects observed session lengths.
	ExpectedRemainingTurns int
	// TierUpgradeEnabled overturns an EV stay when fresh is strictly higher
	// tier than pin. Upgrade-only by design.
	TierUpgradeEnabled bool
}

// Inputs is the full per-turn input to Decide.
type Inputs struct {
	Pin                  sessionpin.Pin
	Fresh                router.Decision
	EstimatedInputTokens int
	AvailableModels      map[string]struct{}
	// PinCacheCold reports that the pin's upstream prompt cache has lapsed —
	// no turn completed within the pinned provider's cache TTL. The proxy
	// computes this (it owns the clock); the planner stays a pure function.
	// When true, the EV math prices both sides uncached. The zero value means
	// "assume warm", preserving the original cache-discounted behavior for any
	// caller that does not supply warmth information.
	PinCacheCold bool
}

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

// Decide returns the planner verdict for this turn.
func Decide(in Inputs, cfg EVConfig) Decision {
	if in.Pin.Model == "" {
		return Decision{Outcome: OutcomeSwitch, Reason: ReasonNoPin}
	}

	if in.Fresh.Model == in.Pin.Model {
		return Decision{Outcome: OutcomeStay, Reason: ReasonSameModel}
	}

	// Pin's model no longer routable: switch regardless of EV.
	// nil AvailableModels means "no filter" — preserve pin.
	if in.AvailableModels != nil {
		if _, ok := in.AvailableModels[in.Pin.Model]; !ok {
			return Decision{Outcome: OutcomeSwitch, Reason: ReasonPinModelMissing}
		}
	}

	// No completed turn yet: no evidence upstream cache is warm.
	if in.Pin.LastTurnEndedAt.IsZero() {
		return Decision{Outcome: OutcomeStay, Reason: ReasonNoPriorUsage}
	}

	pinPrice, ok1 := catalog.PrimaryPriceFor(in.Pin.Model)
	freshPrice, ok2 := catalog.PrimaryPriceFor(in.Fresh.Model)
	if !ok1 || !ok2 {
		return Decision{Outcome: OutcomeStay, Reason: ReasonPricingMissing}
	}

	tokens := float64(in.EstimatedInputTokens)
	// Per-model cache-read multipliers scale savings: only the cache-read
	// portion of per-turn delta accrues over the horizon — but only while the
	// pin's cache is warm. A cold pin earns no discount and switching evicts
	// nothing (both sides pay one cold prefill), so price both uncached and
	// let raw economics and the tier guard decide.
	pinMult, freshMult := 1.0, 1.0
	var evictionCost float64
	if !in.PinCacheCold {
		pinMult = pinPrice.EffectiveCacheReadMultiplier()
		freshMult = freshPrice.EffectiveCacheReadMultiplier()
		evictionCost = freshPrice.InputUSDPer1M * tokens * (1 - freshMult) / 1e6
	}
	savingsPerTurn := (pinPrice.InputUSDPer1M*pinMult - freshPrice.InputUSDPer1M*freshMult) * tokens / 1e6
	expectedSavings := savingsPerTurn * float64(cfg.ExpectedRemainingTurns)

	d := Decision{
		ExpectedSavingsUSD: expectedSavings,
		EvictionCostUSD:    evictionCost,
		ThresholdUSD:       cfg.ThresholdUSD,
		PinCacheCold:       in.PinCacheCold,
	}
	switch {
	case expectedSavings-evictionCost > cfg.ThresholdUSD:
		d.Outcome = OutcomeSwitch
		d.Reason = ReasonEVPositive
	case cfg.TierUpgradeEnabled && tierUpgrade(in.Pin.Model, in.Fresh.Model):
		d.Outcome = OutcomeSwitch
		d.Reason = ReasonTierUpgrade
	default:
		d.Outcome = OutcomeStay
		d.Reason = ReasonEVNegative
	}
	return d
}

// tierUpgrade reports whether fresh is strictly higher tier than pin.
// Unknown on either side disables the guard.
func tierUpgrade(pin, fresh string) bool {
	pinTier := catalog.TierFor(pin)
	freshTier := catalog.TierFor(fresh)
	if pinTier == catalog.TierUnknown || freshTier == catalog.TierUnknown {
		return false
	}
	return freshTier > pinTier
}
