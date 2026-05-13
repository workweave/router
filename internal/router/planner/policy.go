// Package planner decides, per turn, whether to stay on a session's
// pinned model (preserving the upstream prompt cache) or switch to the
// scorer's fresh recommendation. The verdict is a pure function of the
// pin row, the fresh decision, an estimated input-token count, and the
// currently-available model set; no I/O happens here.
//
// The EV math compares:
//
//	expected savings = (Δ input $/M-tok) × tokens × remaining-turn horizon
//	eviction cost    = fresh model input $/M-tok × tokens × (1 - cache-read multiplier)
//
// and switches only when (expected savings - eviction cost) exceeds a
// configurable threshold. The full spec lives in the Prism-style cache-
// aware routing plan; this file is the executable form.
package planner

import (
	"workweave/router/internal/router"
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
	ReasonNoPin             = "no_pin"
	ReasonSameModel         = "same_model"
	ReasonPinModelMissing   = "pin_model_missing"
	ReasonNoPriorUsage      = "no_prior_usage"
	ReasonPricingMissing    = "pricing_missing"
	ReasonEVPositive        = "ev_positive"
	ReasonEVNegative        = "ev_negative"
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
	// retired): we have to switch regardless of EV.
	if _, ok := in.AvailableModels[in.Pin.Model]; !ok {
		return Decision{Outcome: OutcomeSwitch, Reason: ReasonPinModelMissing}
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
	savingsPerTurn := (pinPrice.InputUSDPer1M - freshPrice.InputUSDPer1M) * tokens / 1e6
	evictionCost := freshPrice.InputUSDPer1M * tokens * (1 - pricing.CacheReadMultiplier) / 1e6
	expectedSavings := savingsPerTurn * float64(cfg.ExpectedRemainingTurns)

	d := Decision{
		ExpectedSavingsUSD: expectedSavings,
		EvictionCostUSD:    evictionCost,
		ThresholdUSD:       cfg.ThresholdUSD,
	}
	if expectedSavings-evictionCost > cfg.ThresholdUSD {
		d.Outcome = OutcomeSwitch
		d.Reason = ReasonEVPositive
	} else {
		d.Outcome = OutcomeStay
		d.Reason = ReasonEVNegative
	}
	return d
}
