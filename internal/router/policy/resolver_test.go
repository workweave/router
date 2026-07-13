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

func TestResolverRejectsNonExtendedModelAboveCatalogWindow(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-haiku-4-5"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{EstimatedInputTokens: catalog.ContextWindowFor("claude-haiku-4-5") + 1})

	assert.Empty(t, resolved.Candidates)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "claude-haiku-4-5",
		RosterID:  "claude-haiku-4-5",
		Reason:    policy.ExclusionContextWindow,
	})
}

func TestResolverRejectsExtendedContextModelAboveEffectiveWindow(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	// CapExtendedContext expands eligibility to 1M; above that the model must
	// still be excluded so the fix does not invent infinite headroom.
	resolved := resolver.Resolve(router.Request{EstimatedInputTokens: 1_000_001})

	assert.Empty(t, resolved.Candidates)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "claude-opus-4-8",
		RosterID:  "claude-opus-4-8",
		Reason:    policy.ExclusionContextWindow,
	})
}

// TestResolverKeepsExtendedContextModelAboveCatalogWindow is the live-bug
// regression: CapExtendedContext models (catalog 200K, dispatch 1M) must remain
// candidates when requiredContextTokens is catalogCW+1 (200_001). Filtering on
// catalog.ContextWindowFor alone wrongly excludes them.
func TestResolverKeepsExtendedContextModelAboveCatalogWindow(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)

	resolved := resolver.Resolve(router.Request{
		EstimatedInputTokens: catalog.ContextWindowFor("claude-opus-4-8") + 1,
	})

	assert.Equal(t, []string{"claude-opus-4-8"}, resolved.CandidateModels(),
		"opus-4-8 must stay eligible at catalogCW+1 (serves at 1M via CapExtendedContext)")
	assert.Empty(t, resolved.Diagnostics)
	if assert.Len(t, resolved.Candidates, 1) {
		assert.Equal(t, 1_000_000, resolved.Candidates[0].Capabilities.ContextWindow,
			"candidate capabilities must advertise the effective 1M window, not catalog 200K")
	}
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
		set("claude-haiku-4-5"),
		set(providers.ProviderAnthropic),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)
	expectedOutputTokens := 2_000

	resolved := resolver.Resolve(router.Request{
		EstimatedInputTokens: catalog.ContextWindowFor("claude-haiku-4-5") - 1_000,
		RoutingKnobs:         &router.Overrides{ExpectedOutputTokens: &expectedOutputTokens},
	})

	assert.Empty(t, resolved.Candidates)
	assert.Contains(t, resolved.Diagnostics, policy.Diagnostic{
		CatalogID: "claude-haiku-4-5",
		RosterID:  "claude-haiku-4-5",
		Reason:    policy.ExclusionContextWindow,
	})
}

func set(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
