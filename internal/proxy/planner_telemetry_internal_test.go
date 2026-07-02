package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/planner"
)

// Every planner_* field stays nil/empty when the planner did not run.
func TestApplyPlannerTelemetry_SkippedLeavesFieldsNull(t *testing.T) {
	t.Parallel()
	var p InsertTelemetryParams
	applyPlannerTelemetry(&p, turnLoopResult{})

	assert.Empty(t, p.PlannerOutcome)
	assert.Empty(t, p.PlannerReason)
	assert.Empty(t, p.PlannerPinModel)
	assert.Nil(t, p.PlannerExpectedSavingsUSD)
	assert.Nil(t, p.PlannerEvictionCostUSD)
	assert.Nil(t, p.PlannerThresholdUSD)
	assert.Nil(t, p.PlannerPinCacheCold)
}

// A no_pin verdict must leave planner_pin_model NULL even when res.PinModel
// carries a stale value from a pin a turn-loop guard dropped after the lookup.
func TestApplyPlannerTelemetry_NoPinLeavesStalePinModelNull(t *testing.T) {
	t.Parallel()
	res := turnLoopResult{
		PinModel: "claude-opus-4-7",
		PlannerDecision: planner.Decision{
			Outcome: planner.OutcomeSwitch,
			Reason:  planner.ReasonNoPin,
		},
	}
	var p InsertTelemetryParams
	applyPlannerTelemetry(&p, res)

	assert.Equal(t, "switch", p.PlannerOutcome)
	assert.Equal(t, planner.ReasonNoPin, p.PlannerReason)
	assert.Empty(t, p.PlannerPinModel, "no_pin rows must not carry a dropped pin's model")
	assert.Nil(t, p.PlannerExpectedSavingsUSD)
}

// Early-return verdicts persist outcome/reason/pin-model but leave the USD
// and warmth columns NULL — the EV math never ran.
func TestApplyPlannerTelemetry_EarlyReturnLeavesEVFieldsNull(t *testing.T) {
	t.Parallel()
	res := turnLoopResult{
		PinModel: "claude-opus-4-7",
		PlannerDecision: planner.Decision{
			Outcome: planner.OutcomeStay,
			Reason:  planner.ReasonSameModel,
		},
	}
	var p InsertTelemetryParams
	applyPlannerTelemetry(&p, res)

	assert.Equal(t, "stay", p.PlannerOutcome)
	assert.Equal(t, planner.ReasonSameModel, p.PlannerReason)
	assert.Equal(t, "claude-opus-4-7", p.PlannerPinModel)
	assert.Nil(t, p.PlannerExpectedSavingsUSD, "USD fields must stay NULL on early returns")
	assert.Nil(t, p.PlannerEvictionCostUSD)
	assert.Nil(t, p.PlannerThresholdUSD)
	assert.Nil(t, p.PlannerPinCacheCold)
}

// A STAY verdict persists its full EV breakdown and the pinned from-model.
func TestApplyPlannerTelemetry_StayRecordsEVBreakdown(t *testing.T) {
	t.Parallel()
	res := turnLoopResult{
		PinModel: "claude-opus-4-7",
		PlannerDecision: planner.Decision{
			Outcome:            planner.OutcomeStay,
			Reason:             planner.ReasonEVNegative,
			ExpectedSavingsUSD: -0.063,
			EvictionCostUSD:    0.225,
			ThresholdUSD:       0.001,
			PinCacheCold:       false,
			EVComputed:         true,
		},
	}
	var p InsertTelemetryParams
	applyPlannerTelemetry(&p, res)

	assert.Equal(t, "stay", p.PlannerOutcome)
	assert.Equal(t, planner.ReasonEVNegative, p.PlannerReason)
	assert.Equal(t, "claude-opus-4-7", p.PlannerPinModel)
	require.NotNil(t, p.PlannerExpectedSavingsUSD)
	assert.InDelta(t, -0.063, *p.PlannerExpectedSavingsUSD, 1e-9)
	require.NotNil(t, p.PlannerEvictionCostUSD)
	assert.InDelta(t, 0.225, *p.PlannerEvictionCostUSD, 1e-9)
	require.NotNil(t, p.PlannerThresholdUSD)
	assert.InDelta(t, 0.001, *p.PlannerThresholdUSD, 1e-9)
	require.NotNil(t, p.PlannerPinCacheCold)
	assert.False(t, *p.PlannerPinCacheCold)
}

// A SWITCH verdict preserves the pinned from-model (decision_model names the
// switched-to model).
func TestApplyPlannerTelemetry_SwitchPreservesFromModel(t *testing.T) {
	t.Parallel()
	res := turnLoopResult{
		PinModel: "claude-opus-4-7",
		PlannerDecision: planner.Decision{
			Outcome:            planner.OutcomeSwitch,
			Reason:             planner.ReasonEVPositive,
			ExpectedSavingsUSD: 0.063,
			EvictionCostUSD:    0.036,
			ThresholdUSD:       0.001,
			PinCacheCold:       true,
			EVComputed:         true,
		},
	}
	var p InsertTelemetryParams
	applyPlannerTelemetry(&p, res)

	assert.Equal(t, "switch", p.PlannerOutcome)
	assert.Equal(t, planner.ReasonEVPositive, p.PlannerReason)
	assert.Equal(t, "claude-opus-4-7", p.PlannerPinModel)
	require.NotNil(t, p.PlannerPinCacheCold)
	assert.True(t, *p.PlannerPinCacheCold)
}
