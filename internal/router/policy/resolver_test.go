package policy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
)

func catalogRosterID(model catalog.Model) string { return model.ID }

func TestManagedResolverUsesCurrentProvidersAndNeverOpenRouter(t *testing.T) {
	resolver := policy.NewResolver(
		set("deepseek/deepseek-v4-pro", "xiaomi/mimo-v2.5-pro"),
		set(providers.ProviderFireworks, providers.ProviderOpenRouter),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{})

	require.Len(t, resolved.Candidates, 1)
	assert.Equal(t, "deepseek/deepseek-v4-pro", resolved.Candidates[0].CatalogID)
	assert.Equal(t, providers.ProviderFireworks, resolved.Candidates[0].Provider)
	assert.Equal(t, "accounts/fireworks/models/deepseek-v4-pro", resolved.Candidates[0].UpstreamID)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "xiaomi/mimo-v2.5-pro",
		RosterID:  "xiaomi/mimo-v2.5-pro",
		Reason:    policy.ExclusionProviderPolicy,
	})
}

func TestResolverDefaultsUpstreamIDToCatalogID(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{})

	require.Len(t, resolved.Candidates, 1)
	assert.Equal(t, "claude-opus-4-8", resolved.Candidates[0].UpstreamID)
	assert.Equal(t, resolved.Candidates[0].RosterID, resolved.Candidates[0].ArmID)
}

func TestArmResolverEnumeratesEachAllowedProviderBinding(t *testing.T) {
	resolver := policy.NewArmResolver(
		set("deepseek/deepseek-v4-pro"),
		set(providers.ProviderMakora, providers.ProviderFireworks),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{})

	require.Len(t, resolved.Candidates, 2)
	assert.Equal(t, "deepseek/deepseek-v4-pro", resolved.Candidates[0].RosterID)
	assert.Equal(t, "deepseek/deepseek-v4-pro", resolved.Candidates[1].RosterID)
	assert.NotEqual(t, resolved.Candidates[0].ArmID, resolved.Candidates[1].ArmID)
	assert.Empty(t, resolved.ByRosterID)
	assert.Equal(t, map[string]string{
		resolved.Candidates[0].ArmID: resolved.Candidates[0].Provider,
		resolved.Candidates[1].ArmID: resolved.Candidates[1].Provider,
	}, resolved.CandidateArmProviders())
	assert.Equal(t, map[string]float32{
		resolved.Candidates[0].ArmID: 0.1,
		resolved.Candidates[1].ArmID: 0.2,
	}, resolved.ArmCandidateScores(map[string]float32{
		resolved.Candidates[0].ArmID: 0.1,
		resolved.Candidates[1].ArmID: 0.2,
	}))
	for _, candidate := range resolved.Candidates {
		binding, ok := resolved.BindingForSelection(candidate.ArmID, "")
		require.True(t, ok)
		assert.Equal(t, candidate.Provider, binding.Provider)
		assert.Equal(t, candidate.UpstreamID, binding.UpstreamID)
	}
}

func TestArmResolverRejectsRosterOnlySelectionForThreeBindings(t *testing.T) {
	resolver := policy.NewArmResolver(
		set("deepseek/deepseek-v4-pro"),
		set(
			providers.ProviderMakora,
			providers.ProviderTogether,
			providers.ProviderFireworks,
			providers.ProviderOpenRouter,
		),
		func(catalog.Model) string { return "shared/arm" },
		policy.ProviderPolicy{},
	)

	resolved := resolver.Resolve(router.Request{})

	require.Len(t, resolved.Candidates, 4)
	assert.Empty(t, resolved.ByRosterID)
	_, ok := resolved.BindingForSelection("", "shared/arm")
	assert.False(t, ok)
}

func TestResolverAppliesHardFiltersAndPreferenceRanks(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "gpt-5.5"),
		set(providers.ProviderAnthropic, providers.ProviderOpenAI),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{
		EnabledProviders: set(providers.ProviderAnthropic),
		PreferredModels:  []string{"gpt-5.5", "claude-opus-4-8"},
	})

	require.Len(t, resolved.Candidates, 1)
	assert.Equal(t, "claude-opus-4-8", resolved.Candidates[0].CatalogID)
	require.NotNil(t, resolved.Candidates[0].PreferenceRank)
	assert.Equal(t, 1, *resolved.Candidates[0].PreferenceRank)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "gpt-5.5",
		RosterID:  "gpt-5.5",
		Reason:    policy.ExclusionNoProvider,
	})
}

func TestResolverBuildsMappingOnlyFromFinalSoftFilteredPool(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "deepseek/deepseek-v4-flash"),
		set(providers.ProviderAnthropic, providers.ProviderMakora),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{HasImages: true})

	assert.Equal(t, []string{"claude-opus-4-8"}, resolved.CandidateModels())
	_, leaked := resolved.ByRosterID["deepseek/deepseek-v4-flash"]
	assert.False(t, leaked)
}

func TestResolverRejectsAmbiguousRosterMappings(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "gpt-5.5"),
		set(providers.ProviderAnthropic, providers.ProviderOpenAI),
		func(catalog.Model) string { return "shared/arm" },
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{})

	assert.Empty(t, resolved.Candidates)
	assert.Empty(t, resolved.ByRosterID)
	assert.Len(t, resolved.Diagnostics, 2)
}

func TestResolverRejectsCandidatesThatCannotFitEstimatedInput(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{EstimatedInputTokens: catalog.ContextWindowFor("claude-opus-4-8") + 1})

	assert.Empty(t, resolved.Candidates)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "claude-opus-4-8",
		RosterID:  "claude-opus-4-8",
		Reason:    policy.ExclusionContextWindow,
	})
}

func TestResolverAllowsExactContextFit(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{EstimatedInputTokens: catalog.ContextWindowFor("claude-opus-4-8")})

	assert.Equal(t, []string{"claude-opus-4-8"}, resolved.CandidateModels())
	assert.Empty(t, resolved.Diagnostics)
}

func TestResolverIncludesExpectedOutputInContextBudget(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)
	expectedOutputTokens := 2_000

	resolved := resolver.Resolve(router.Request{
		EstimatedInputTokens: catalog.ContextWindowFor("claude-opus-4-8") - 1_000,
		RoutingKnobs:         &router.Overrides{ExpectedOutputTokens: &expectedOutputTokens},
	})

	assert.Empty(t, resolved.Candidates)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "claude-opus-4-8",
		RosterID:  "claude-opus-4-8",
		Reason:    policy.ExclusionContextWindow,
	})
}

func TestResolverIncludesLiveCandidateEconomics(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)
	expectedOutputTokens := 500

	resolved := resolver.Resolve(router.Request{
		EstimatedInputTokens: 1_000,
		RoutingKnobs:         &router.Overrides{ExpectedOutputTokens: &expectedOutputTokens},
		SubsidizedModelCostFactor: map[string]float64{
			"claude-opus-4-8": 0.25,
		},
	})

	require.Len(t, resolved.Candidates, 1)
	candidate := resolved.Candidates[0]
	assert.Equal(t, 0.1, candidate.CacheReadMultiplier)
	assert.Equal(t, 0.25, candidate.MarginalCostFactor)
	assert.Equal(t, 1.25, candidate.EffectiveInputUSDPer1M)
	assert.Equal(t, 6.25, candidate.EffectiveOutputUSDPer1M)
	assert.InDelta(t, candidate.EstimatedCostUSD*0.25, candidate.EffectiveEstimatedCostUSD, 1e-12)
}

func set(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
