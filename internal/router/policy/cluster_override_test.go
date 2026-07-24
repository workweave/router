package policy_test

import (
	"testing"

	"workweave/router/internal/router/policy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvedFor builds a ResolvedCandidates whose catalog->roster mapping mirrors
// the frozen HMM aliasing (anthropic/<model> etc.), so overrides expressed in
// catalog IDs intersect the sidecar's roster-ID arms.
func resolvedFor(pairs map[string]string) policy.ResolvedCandidates {
	resolved := policy.ResolvedCandidates{
		ByRosterID: map[string]policy.Binding{},
		ByArmID:    map[string]policy.Binding{},
	}
	for catalogID, rosterID := range pairs {
		resolved.Candidates = append(resolved.Candidates, policy.Candidate{
			ArmID:     rosterID,
			RosterID:  rosterID,
			CatalogID: catalogID,
		})
		resolved.ByRosterID[rosterID] = policy.Binding{ArmID: rosterID, CatalogID: catalogID}
		resolved.ByArmID[rosterID] = policy.Binding{ArmID: rosterID, CatalogID: catalogID}
	}
	return resolved
}

func TestApplyClusterArmOverrides_NoOverrideKeepsSidecar(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus", "anthropic/fable"}},
	}
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus", "fable": "anthropic/fable"})

	out := policy.ApplyClusterArmOverrides(nil, ranked, resolved, "anthropic/opus")
	assert.False(t, out.Applied, "no overrides must leave selection to the sidecar")
}

func TestApplyClusterArmOverrides_ReorderPromotesRunnerUp(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus", "anthropic/fable"}},
	}
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus", "fable": "anthropic/fable"})
	// Key reorders the cluster so fable is now first priority.
	overrides := map[string][]string{"maximum": {"fable", "opus"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/opus")
	require.True(t, out.Applied)
	assert.Equal(t, "anthropic/fable", out.RosterID, "override order must decide the served arm")
	assert.True(t, out.Changed, "promoting the runner-up is a change from the sidecar pick")
	assert.Equal(t, "maximum", out.Group)
}

func TestApplyClusterArmOverrides_RemovalFallsToNextEligible(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus", "anthropic/fable"}},
	}
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus", "fable": "anthropic/fable"})
	// Key removes opus from the cluster, leaving fable.
	overrides := map[string][]string{"maximum": {"fable"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/opus")
	require.True(t, out.Applied)
	assert.Equal(t, "anthropic/fable", out.RosterID)
}

func TestApplyClusterArmOverrides_EmptyTopGroupFallsThrough(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus"}},
		{Group: "fast", EligibleArms: []string{"anthropic/haiku"}},
	}
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus", "haiku": "anthropic/haiku"})
	// Key removes the only arm in the top group by listing a model that is not
	// an eligible candidate; the next group's arm must serve.
	overrides := map[string][]string{"maximum": {"not-deployed-model"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/opus")
	require.True(t, out.Applied)
	assert.Equal(t, "anthropic/haiku", out.RosterID)
	assert.Equal(t, "fast", out.Group)
}

func TestApplyClusterArmOverrides_AddsModelNotInArtifactArms(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus"}},
	}
	// gpt5 is a resolved candidate but was never in the artifact's maximum arms.
	resolved := resolvedFor(map[string]string{
		"opus": "anthropic/opus",
		"gpt5": "openai/gpt5",
	})
	overrides := map[string][]string{"maximum": {"gpt5", "opus"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/opus")
	require.True(t, out.Applied)
	assert.Equal(t, "openai/gpt5", out.RosterID, "an added model must be servable when it is a resolved candidate")
}

func TestApplyClusterArmOverrides_AllEmptyKeepsSidecar(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus"}},
	}
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus"})
	// Override names only ineligible models, emptying every ranked group.
	overrides := map[string][]string{"maximum": {"ghost"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/opus")
	assert.False(t, out.Applied, "emptied overrides must fail open to the sidecar selection")
}

func TestApplyClusterArmOverrides_ReturnsResolverArmID(t *testing.T) {
	ranked := []policy.PreviewGroup{
		{Group: "maximum", EligibleArms: []string{"anthropic/opus"}},
	}
	// Arm ID distinct from roster ID (arm-enumerating resolver shape).
	resolved := policy.ResolvedCandidates{
		Candidates: []policy.Candidate{{
			ArmID:     "anthropic/opus#0",
			RosterID:  "anthropic/opus",
			CatalogID: "opus",
		}},
	}
	overrides := map[string][]string{"maximum": {"opus"}}

	out := policy.ApplyClusterArmOverrides(overrides, ranked, resolved, "anthropic/other")
	require.True(t, out.Applied)
	assert.Equal(t, "anthropic/opus", out.RosterID)
	assert.Equal(t, "anthropic/opus#0", out.ArmID, "the resolved arm ID must be returned so binding goes through ByArmID")
}

func TestApplyClusterArmOverrides_OldSidecarNoFallbackKeepsSidecar(t *testing.T) {
	resolved := resolvedFor(map[string]string{"opus": "anthropic/opus"})
	overrides := map[string][]string{"maximum": {"opus"}}

	out := policy.ApplyClusterArmOverrides(overrides, nil, resolved, "anthropic/opus")
	assert.False(t, out.Applied, "no ranked fallback (old sidecar) must fail open")
}
