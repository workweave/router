package proxy

import (
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
)

func floatPtr(v float64) *float64 { return &v }

func TestComposePhaseKnobs_PerRequestWins(t *testing.T) {
	base := router.Overrides{Alpha: floatPtr(0.80), SpeedWeight: floatPtr(0.0)}

	t.Run("nil request knobs returns phase defaults", func(t *testing.T) {
		got := composePhaseKnobs(base, nil)
		assert.Equal(t, 0.80, *got.Alpha)
		assert.Equal(t, 0.0, *got.SpeedWeight)
	})

	t.Run("per-request alpha overrides phase default, gaps filled", func(t *testing.T) {
		got := composePhaseKnobs(base, &router.Overrides{Alpha: floatPtr(0.10)})
		assert.Equal(t, 0.10, *got.Alpha, "per-request alpha must win")
		assert.Equal(t, 0.0, *got.SpeedWeight, "phase default fills the unset field")
	})

	t.Run("does not mutate base", func(t *testing.T) {
		_ = composePhaseKnobs(base, &router.Overrides{Alpha: floatPtr(0.10)})
		assert.Equal(t, 0.80, *base.Alpha, "base must be unchanged")
	})
}

func TestApplyPlanningFloor_ExcludesBelowFloor(t *testing.T) {
	s := &Service{
		phaseRouting:    PhaseRoutingConfig{PlanningFloor: catalog.TierHigh},
		availableModels: map[string]struct{}{"claude-opus-4-8": {}, "claude-sonnet-4-6": {}, "claude-haiku-4-5": {}},
	}
	req := router.Request{}
	out := s.applyPlanningFloor(req)

	_, opusExcluded := out.ExcludedModels["claude-opus-4-8"]
	_, sonnetExcluded := out.ExcludedModels["claude-sonnet-4-6"]
	_, haikuExcluded := out.ExcludedModels["claude-haiku-4-5"]
	assert.False(t, opusExcluded, "High-tier model must remain eligible")
	assert.True(t, sonnetExcluded, "Mid-tier model must be excluded under a High floor")
	assert.True(t, haikuExcluded, "Low-tier model must be excluded under a High floor")
}

func TestApplyPlanningFloor_SoftFallbackWhenNoHighAvailable(t *testing.T) {
	s := &Service{
		phaseRouting:    PhaseRoutingConfig{PlanningFloor: catalog.TierHigh},
		availableModels: map[string]struct{}{"claude-haiku-4-5": {}},
	}
	req := router.Request{}
	out := s.applyPlanningFloor(req)
	assert.Empty(t, out.ExcludedModels, "no at-or-above-floor model available: floor must be skipped, not 503")
}

func TestApplyPlanningFloor_DoesNotMutateCallerExclusions(t *testing.T) {
	s := &Service{
		phaseRouting:    PhaseRoutingConfig{PlanningFloor: catalog.TierHigh},
		availableModels: map[string]struct{}{"claude-opus-4-8": {}, "claude-sonnet-4-6": {}},
	}
	original := map[string]struct{}{"installation-excluded": {}}
	req := router.Request{ExcludedModels: original}

	out := s.applyPlanningFloor(req)

	assert.Len(t, original, 1, "caller's ExcludedModels map must not be mutated")
	_, kept := out.ExcludedModels["installation-excluded"]
	assert.True(t, kept, "pre-existing exclusion must carry through")
}

func TestApplyPlanningFloor_DisabledFloorIsNoop(t *testing.T) {
	s := &Service{
		phaseRouting:    PhaseRoutingConfig{PlanningFloor: catalog.TierUnknown},
		availableModels: map[string]struct{}{"claude-opus-4-8": {}, "claude-sonnet-4-6": {}},
	}
	out := s.applyPlanningFloor(router.Request{})
	assert.Empty(t, out.ExcludedModels)
}
