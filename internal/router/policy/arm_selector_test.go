package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/policy"
)

func newSelectorAdapter(result policy.Result) *policy.SidecarRouter {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "claude-sonnet-5"),
		set(providers.ProviderAnthropic),
		func(model catalog.Model) string { return "anthropic/" + model.ID },
		policy.ManagedProviderPolicy(),
	)
	return policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy:    router.StrategyHMM,
		Unavailable: errors.New("selection unavailable"),
	}, &recordingPolicy{result: result}, resolver)
}

func classifierOnlyResult() policy.Result {
	return policy.Result{
		SchemaVersion: policy.SchemaVersionV3,
		RouteID:       "route-classifier",
		Score:         0.8,
		PolicyGroup:   "maximum",
		RankedFallback: []policy.PreviewGroup{{
			Group:        "maximum",
			Probability:  0.8,
			RosterArms:   []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
			EligibleArms: []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
		}},
	}
}

func TestArmSelectorPickIsServed(t *testing.T) {
	adapter := newSelectorAdapter(classifierOnlyResult())
	qualityBias := 0.2
	adapter.WithArmSelector(func(_ context.Context, input policy.SelectionInput) (policy.SelectionPick, error) {
		assert.Equal(t, "maximum", input.ClassifierGroup)
		assert.ElementsMatch(t, []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"}, input.CandidateRosterIDs)
		require.NotNil(t, input.QualityBias)
		assert.Equal(t, qualityBias, *input.QualityBias)
		return policy.SelectionPick{
			Group: "maximum",
			Arm:   "anthropic/claude-sonnet-5",
			ArmScoresByGroup: map[string]map[string]float32{
				"maximum": {"anthropic/claude-sonnet-5": 42},
			},
		}, nil
	})

	decision, err := adapter.Route(context.Background(), router.Request{
		RoutingKnobs: &router.Overrides{QualityBias: &qualityBias},
	})

	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
	assert.Contains(t, decision.Reason, ":go_selection")
	require.NotNil(t, decision.Metadata)
	assert.Equal(t, "anthropic/claude-sonnet-5", decision.Metadata.SelectedRosterArmID)
	assert.Equal(t, "maximum", decision.Metadata.PolicyGroup)
	assert.Equal(t, float32(42), decision.Metadata.ArmScores["anthropic/claude-sonnet-5"])
}

func TestArmSelectorErrorFailsTheTurn(t *testing.T) {
	adapter := newSelectorAdapter(classifierOnlyResult())
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		return policy.SelectionPick{}, errors.New("no eligible arm")
	})

	_, err := adapter.Route(context.Background(), router.Request{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "selection unavailable",
		"a failed selection must surface as the strategy's unavailable sentinel, not a sidecar-picked arm")
}

func TestArmSelectorForceClusterExhaustionIsCallerError(t *testing.T) {
	result := classifierOnlyResult()
	result.RankedFallback = append(result.RankedFallback, policy.PreviewGroup{
		Group:        "low",
		Probability:  0.2,
		EligibleArms: []string{"anthropic/claude-sonnet-5"},
	})
	adapter := newSelectorAdapter(result)
	adapter.WithArmSelector(func(_ context.Context, input policy.SelectionInput) (policy.SelectionPick, error) {
		require.Len(t, input.RankedFallback, 1)
		assert.Equal(t, "low", input.RankedFallback[0].Group)
		return policy.SelectionPick{}, policy.ErrNoEligibleArm
	})

	_, err := adapter.Route(context.Background(), router.Request{ForceCluster: "low"})

	require.ErrorIs(t, err, policy.ErrForcedClusterUnservable)
	assert.NotContains(t, err.Error(), "selection unavailable")
	assert.Contains(t, err.Error(), "low")
}

func TestArmSelectorRejectsLegacySchema(t *testing.T) {
	result := classifierOnlyResult()
	result.SchemaVersion = policy.SchemaVersionV1
	result.Model = "anthropic/claude-opus-4-8"

	adapter := newSelectorAdapter(result)
	called := false
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		called = true
		return policy.SelectionPick{Group: "maximum", Arm: "anthropic/claude-opus-4-8"}, nil
	})

	_, err := adapter.Route(context.Background(), router.Request{})

	require.Error(t, err)
	assert.False(t, called, "a legacy response must be rejected before selection runs")
	assert.Contains(t, err.Error(), policy.SchemaVersionV3)
}

func TestArmSelectorNegotiatesV3(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		func(model catalog.Model) string { return "anthropic/" + model.ID },
		policy.ManagedProviderPolicy(),
	)
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy:    router.StrategyHMM,
		Unavailable: errors.New("selection unavailable"),
	}, &recordingPolicy{result: classifierOnlyResult()}, resolver)

	assert.Equal(t, policy.SchemaVersionV1, resolver.SchemaVersion())
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		return policy.SelectionPick{Group: "maximum", Arm: "anthropic/claude-opus-4-8"}, nil
	})
	assert.Equal(t, policy.SchemaVersionV3, resolver.SchemaVersion())
}

func TestArmSelectorYieldsToClusterOverride(t *testing.T) {
	adapter := newSelectorAdapter(classifierOnlyResult())
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		return policy.SelectionPick{Group: "maximum", Arm: "anthropic/claude-opus-4-8"}, nil
	})

	decision, err := adapter.Route(context.Background(), router.Request{
		ClusterArmOverrides: map[string][]string{
			"maximum": {"claude-sonnet-5", "claude-opus-4-8"},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Contains(t, decision.Reason, ":cluster_override")
}

func TestArmSelectorSurvivesOverridesOmittingWinningGroup(t *testing.T) {
	adapter := newSelectorAdapter(classifierOnlyResult())
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		return policy.SelectionPick{Group: "maximum", Arm: "anthropic/claude-sonnet-5"}, nil
	})

	// A partial per-key map that configures only an unrelated cluster must not
	// suppress Go selection for the served group.
	decision, err := adapter.Route(context.Background(), router.Request{
		ClusterArmOverrides: map[string][]string{
			"minimal": {"claude-sonnet-5"},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Contains(t, decision.Reason, ":go_selection")
}

func TestArmSelectorForceClusterUsesFinalGroupScores(t *testing.T) {
	result := classifierOnlyResult()
	result.RankedFallback = append(result.RankedFallback, policy.PreviewGroup{
		Group:        "low",
		Probability:  0.2,
		RosterArms:   []string{"anthropic/claude-sonnet-5"},
		EligibleArms: []string{"anthropic/claude-sonnet-5"},
	})
	adapter := newSelectorAdapter(result)
	adapter.WithArmSelector(func(_ context.Context, _ policy.SelectionInput) (policy.SelectionPick, error) {
		return policy.SelectionPick{
			Group: "maximum",
			Arm:   "anthropic/claude-opus-4-8",
			ArmScoresByGroup: map[string]map[string]float32{
				"maximum": {"anthropic/claude-opus-4-8": 90},
				"low":     {"anthropic/claude-sonnet-5": 20},
			},
		}, nil
	})

	decision, err := adapter.Route(context.Background(), router.Request{
		ForceCluster: "low",
		ClusterArmOverrides: map[string][]string{
			"low": {"claude-sonnet-5"},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, decision.Metadata)
	assert.Equal(t, "low", decision.Metadata.PolicyGroup)
	assert.Equal(t, map[string]float32{"anthropic/claude-sonnet-5": 20}, decision.Metadata.ArmScores)
}

func TestArmSelectorForceClusterPreservesPreferenceRanking(t *testing.T) {
	result := classifierOnlyResult()
	result.RankedFallback = append(result.RankedFallback, policy.PreviewGroup{
		Group:        "low",
		Probability:  0.2,
		RosterArms:   []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
		EligibleArms: []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
	})
	adapter := newSelectorAdapter(result)
	adapter.WithArmSelector(func(_ context.Context, input policy.SelectionInput) (policy.SelectionPick, error) {
		require.Len(t, input.RankedFallback, 1)
		assert.Equal(t, "low", input.ClassifierGroup)
		assert.Equal(t, "low", input.RankedFallback[0].Group)
		return policy.SelectionPick{
			Group: "low",
			Arm:   "anthropic/claude-sonnet-5",
			ArmScoresByGroup: map[string]map[string]float32{
				"low": {"anthropic/claude-opus-4-8": 10, "anthropic/claude-sonnet-5": 20},
			},
		}, nil
	})

	decision, err := adapter.Route(context.Background(), router.Request{ForceCluster: "low"})

	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Contains(t, decision.Reason, ":force_cluster")
	require.NotNil(t, decision.Metadata)
	assert.Equal(t, "low", decision.Metadata.PolicyGroup)
	assert.Equal(t, map[string]float32{
		"anthropic/claude-opus-4-8": 10,
		"anthropic/claude-sonnet-5": 20,
	}, decision.Metadata.ArmScores)
}
