package proxy

import (
	"context"
	"encoding/json"
	"testing"

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

	obs := buildObservationContext(context.Background(), served, fresh)

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

	obs := buildObservationContext(context.Background(), served, router.Decision{})

	assert.Empty(t, obs.FreshDecisionModel)
	assert.Nil(t, obs.FreshCandidateScores)
}

// TestBuildObservationContext_CapturesHMMStrategy verifies that Strategy, RouteID, and Propensity
// survive when the decision carries no candidate-score vector.
func TestBuildObservationContext_CapturesHMMStrategy(t *testing.T) {
	served := router.Decision{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(label=explore)",
		Metadata: &router.RoutingMetadata{
			CandidateModels: []string{"claude-opus-4-8", "claude-haiku-4-5"},
			ChosenScore:     0.71,
			Strategy:        string(router.StrategyHMM),
			RouteID:         "route-abc-123",
			Propensity:      1.0,
		},
	}

	obs := buildObservationContext(context.Background(), served, router.Decision{})

	assert.Equal(t, "hmm", obs.Strategy)
	assert.Equal(t, "route-abc-123", obs.RouteID)
	require.NotNil(t, obs.Propensity, "HMM propensity must survive without a score vector")
	assert.InDelta(t, 1.0, *obs.Propensity, 1e-6)
	// Sidecar sent no candidate-score map, so that column stays NULL.
	assert.Nil(t, obs.CandidateScores)
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

	obs := buildObservationContext(context.Background(), served, router.Decision{})

	assert.Equal(t, string(router.StrategyCluster), obs.Strategy)
	assert.Empty(t, obs.RouteID)
}

// TestBuildObservationContext_StickyHMMRouteIDFromFresh verifies route_id falls back to
// fresh on a sticky HMM turn (served pin has nil metadata; fresh carries the sidecar id
// hmmOutcomeRoute reports against) so the telemetry row joins to that outcome, not NULL.
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
	obs := buildObservationContext(ctx, served, fresh)

	assert.Equal(t, "route-sticky-1", obs.RouteID, "route_id must fall back to fresh on a sticky HMM turn")
	assert.Equal(t, "hmm", obs.Strategy, "active strategy labels the sticky turn even with a metadata-less pin")
}
