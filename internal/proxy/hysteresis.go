package proxy

import (
	"weave-os/router/internal/router"
)

// effortHysteresisThreshold is the minimum WMI-score gap required for an
// effort-only change on the same base model within the same cluster.
const effortHysteresisThreshold = 1.0

// effortHysteresisReason tags the planner reason of a held turn so a bake-off
// can separate held turns from clean stays.
const effortHysteresisReason = "effort_hysteresis"

// effortHysteresisHold returns the incumbent effort to keep serving, or "" to
// let the challenger through. Both sides are looked up in the router's
// preference-adjusted per-arm WII/WPI score map, keyed by roster arm ID
// ("anthropic/claude-opus-5:xhigh") rather than by catalog serving identity.
func effortHysteresisHold(fresh router.Decision, prevServedModel, chosenModel, chosenEffort string) string {
	if fresh.Metadata == nil || len(fresh.Metadata.ArmScores) == 0 {
		return ""
	}
	incumbentEffort, incumbentModel := stripEffortSuffix(prevServedModel)
	if incumbentEffort == "" || chosenEffort == "" || incumbentEffort == chosenEffort {
		return ""
	}
	// Cross-model moves are the planner's call, and a stay can serve a model
	// the fresh arm scores don't describe.
	if chosenModel != incumbentModel || fresh.Model != incumbentModel {
		return ""
	}
	_, armBase := stripEffortSuffix(fresh.Metadata.SelectedRosterArmID)
	if armBase == "" {
		return ""
	}
	challengerScore, ok := fresh.Metadata.ArmScores[armBase+":"+chosenEffort]
	if !ok {
		return ""
	}
	incumbentScore, ok := fresh.Metadata.ArmScores[armBase+":"+incumbentEffort]
	if !ok {
		return ""
	}
	if challengerScore-incumbentScore >= effortHysteresisThreshold {
		return ""
	}
	return incumbentEffort
}

// appendEffortHysteresisReason marks a planner reason as effort-held. The
// planner's own verdict is preserved because it still explains the model.
func appendEffortHysteresisReason(reason string) string {
	if reason == "" {
		return effortHysteresisReason
	}
	return reason + "+" + effortHysteresisReason
}
