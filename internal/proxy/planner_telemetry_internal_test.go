package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router/planner"
)

// applyPlannerTelemetry must leave every planner_* field nil/empty when the
// planner did not run this turn, so skipped turns stay distinct from measured
// zeros in the shadow corpus.
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

// An early-return planner verdict (same_model, no_prior_usage, ...) must
// persist outcome/reason/pin-model — those are facts — but leave the USD and
// warmth columns NULL: the EV math never ran, so their zero values are
// structural, not measurements. Writing them would poison the shadow corpus
// with measured-looking zeros.
func TestApplyPlannerTelemetry_EarlyReturnLeavesEVFieldsNull(t *testing.T) {
	t.Parallel()
	res := turnLoopResult{
		PinModel: "claude-opus-4-7",
		PlannerDecision: planner.Decision{
			Outcome: planner.OutcomeStay,
			Reason:  planner.ReasonSameModel,
			// EVComputed false: Decide early-returned before the EV math.
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

// A planner STAY verdict must be persisted with its full EV breakdown and the
// pinned from-model, including genuinely zero USD values.
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

// A SWITCH verdict must record outcome "switch" and preserve the pinned
// from-model even though the row's decision_model names the switched-to model.
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
