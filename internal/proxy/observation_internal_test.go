package proxy

import (
	"context"
	"encoding/json"
	"testing"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildObservationContext_CapturesFreshOnStay is the load-bearing assertion
// for the hysteresis shadow instrumentation: on a STAY the final decision
// rehydrates from the pin and carries NO metadata (so CandidateScores is NULL),
// but the fresh scorer DID run this turn. buildObservationContext must capture
// the fresh pick + score vector independently so the downgrade opportunity is
// measurable offline.
func TestBuildObservationContext_CapturesFreshOnStay(t *testing.T) {
	// Final (served) decision = the pinned model, no metadata — the STAY shape.
	served := router.Decision{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Reason:   "pin",
	}
	// Fresh scorer recommendation this turn: a cheaper model nearly ties opus.
	fresh := router.Decision{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Reason:   "cluster:v-test",
		Metadata: &router.RoutingMetadata{
			CandidateModels: []string{"claude-opus-4-8", "claude-haiku-4-5"},
			ChosenScore:     0.82,
			CandidateScores: map[string]float32{"claude-opus-4-8": 0.83, "claude-haiku-4-5": 0.82},
			Propensity:      1.0,
		},
	}

	obs := buildObservationContext(context.Background(), served, fresh, CaptureOff)

	// Final-decision columns stay NULL on a STAY (served pin has no metadata).
	assert.Nil(t, obs.CandidateScores, "final CandidateScores must be NULL on a STAY")
	assert.Nil(t, obs.ChosenScore)

	// Fresh columns must be populated from the scorer's recommendation.
	assert.Equal(t, "claude-haiku-4-5", obs.FreshDecisionModel)
	require.NotNil(t, obs.FreshCandidateScores, "fresh score vector must be captured on a STAY")
	var got map[string]float32
	require.NoError(t, json.Unmarshal(obs.FreshCandidateScores, &got))
	assert.InDelta(t, 0.83, got["claude-opus-4-8"], 1e-6)
	assert.InDelta(t, 0.82, got["claude-haiku-4-5"], 1e-6)
}

// TestBuildObservationContext_NoScorerLeavesFreshNull: when the scorer did not
// run (hard-pin / tool_result), fresh is a zero Decision, so the fresh columns
// stay NULL rather than logging a phantom model.
func TestBuildObservationContext_NoScorerLeavesFreshNull(t *testing.T) {
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8", Reason: "tool_result_sc"}

	obs := buildObservationContext(context.Background(), served, router.Decision{}, CaptureOff)

	assert.Empty(t, obs.FreshDecisionModel)
	assert.Nil(t, obs.FreshCandidateScores)
}

// TestBuildObservationContext_CapturesHMMStrategy verifies Strategy, RouteID, and Propensity
// survive when the decision carries Propensity without a candidate-score vector.
func TestBuildObservationContext_CapturesHMMStrategy(t *testing.T) {
	served := router.Decision{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(label=explore)",
		Metadata: &router.RoutingMetadata{
			CandidateModels:      []string{"claude-opus-4-8", "claude-haiku-4-5"},
			ChosenScore:          0.71,
			Strategy:             string(router.StrategyHMM),
			RouteID:              "route-abc-123",
			PolicyRouteKey:       "medium|open",
			PolicyArtifactID:     "hmm-prod",
			PolicyArtifactSHA256: "sha256:abc",
			RosterVersion:        "roster-v2",
			SidecarSchemaVersion: "policy_router_v1",
			DebugRef:             "debug-1",
			Propensity:           1.0,
		},
	}

	ctx := context.WithValue(context.Background(), PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, PolicyDebugEnabledContextKey{}, true)
	ctx = context.WithValue(ctx, PolicyRolloutIDContextKey{}, "rollout-1")
	obs := buildObservationContext(ctx, served, router.Decision{}, CaptureHashed)

	assert.Equal(t, "hmm", obs.Strategy)
	assert.Equal(t, "route-abc-123", obs.RouteID)
	assert.Equal(t, "medium|open", obs.PolicyRouteKey)
	assert.Equal(t, "hmm-prod", obs.PolicyArtifactID)
	assert.Equal(t, "sha256:abc", obs.PolicyArtifactSHA256)
	assert.Equal(t, "roster-v2", obs.RosterVersion)
	assert.Equal(t, "policy_router_v1", obs.SidecarSchemaVersion)
	assert.Equal(t, "rollout-1", obs.RolloutID)
	assert.True(t, obs.TrainingAllowed)
	assert.Equal(t, "hashed", obs.CaptureMode)
	assert.Equal(t, "debug-1", obs.DebugRef)
	require.NotNil(t, obs.Propensity, "HMM propensity must survive without a score vector")
	assert.InDelta(t, 1.0, *obs.Propensity, 1e-6)
	// Sidecar sent no candidate-score map, so that column stays NULL.
	assert.Nil(t, obs.CandidateScores)

	builder := otel.NewAttrBuilder(16)
	obs.applySpanAttrs(builder)
	attrs := make(map[string]any)
	for _, attr := range builder.Build() {
		switch attr.Key {
		case "routing.training_allowed":
			attrs[attr.Key] = attr.Value.GetBoolValue()
		default:
			attrs[attr.Key] = attr.Value.GetStringValue()
		}
	}
	assert.Equal(t, "medium|open", attrs["routing.policy_route_key"])
	assert.Equal(t, "hmm-prod", attrs["routing.policy_artifact_id"])
	assert.Equal(t, "sha256:abc", attrs["routing.policy_artifact_sha256"])
	assert.Equal(t, "roster-v2", attrs["routing.roster_version"])
	assert.Equal(t, "policy_router_v1", attrs["routing.sidecar_schema_version"])
	assert.Equal(t, true, attrs["routing.training_allowed"])
	assert.Equal(t, "hashed", attrs["routing.capture_mode"])
	assert.Equal(t, "rollout-1", attrs["routing.rollout_id"])
	assert.Equal(t, "debug-1", attrs["routing.debug_ref"])
}

func TestBuildObservationContext_SuppressesDebugRefWithoutDebugMode(t *testing.T) {
	served := router.Decision{Metadata: &router.RoutingMetadata{DebugRef: "private-debug-ref"}}

	obs := buildObservationContext(context.Background(), served, router.Decision{}, CaptureOff)

	assert.Empty(t, obs.DebugRef)
	assert.False(t, obs.TrainingAllowed)
	assert.Equal(t, "off", obs.CaptureMode)
}

// TestBuildObservationContext_DefaultsStrategyToActive verifies Strategy falls back to the
// request's active strategy and RouteID stays empty when metadata carries no sidecar strategy.
func TestBuildObservationContext_DefaultsStrategyToActive(t *testing.T) {
	served := router.Decision{
		Provider: "anthropic",
		Model:    "claude-opus-4-8",
		Reason:   "cluster:v-test",
		Metadata: &router.RoutingMetadata{
			CandidateModels: []string{"claude-opus-4-8"},
			ChosenScore:     0.9,
		},
	}

	obs := buildObservationContext(context.Background(), served, router.Decision{}, CaptureOff)

	assert.Equal(t, string(router.StrategyCluster), obs.Strategy)
	assert.Empty(t, obs.RouteID)
}

// TestBuildObservationContext_StickyHMMRouteIDFromFresh verifies route_id falls back to
// fresh.Metadata on a sticky HMM turn so telemetry joins to the same id policyOutcomeRoute reports.
func TestBuildObservationContext_StickyHMMRouteIDFromFresh(t *testing.T) {
	// Served pin — no metadata, the sticky shape.
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8", Reason: "pin"}
	// Fresh HMM re-score this turn carries the correlation id.
	fresh := router.Decision{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(label=explore)",
		Metadata: &router.RoutingMetadata{
			Strategy: string(router.StrategyHMM),
			RouteID:  "route-sticky-1",
		},
	}

	ctx := router.WithStrategy(context.Background(), router.StrategyHMM)
	obs := buildObservationContext(ctx, served, fresh, CaptureOff)

	assert.Equal(t, "route-sticky-1", obs.RouteID, "route_id must fall back to fresh on a sticky HMM turn")
	assert.Equal(t, "hmm", obs.Strategy, "active strategy labels the sticky turn even with a metadata-less pin")
}

// TestBuildObservationContext_HardPinLeavesStrategyNull verifies that turns bypassing routing
// (no served metadata and no fresh re-score) leave strategy NULL, not the session's active strategy.
func TestBuildObservationContext_HardPinLeavesStrategyNull(t *testing.T) {
	served := router.Decision{Provider: "anthropic", Model: "claude-opus-4-8", Reason: "user_forced"}

	// Request opted into HMM, but this turn was hard-pinned and never scored.
	ctx := router.WithStrategy(context.Background(), router.StrategyHMM)
	obs := buildObservationContext(ctx, served, router.Decision{}, CaptureOff)

	assert.Empty(t, obs.Strategy, "hard-pin turn must not claim the active strategy produced it")
	assert.Empty(t, obs.RouteID)
}
