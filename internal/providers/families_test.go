package providers_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
)

// TestEveryProviderHasFamilyAndEnvVar guards the three-map invariant: every
// provider AllProviders reports must have a concrete translation family and a
// non-empty deployment env-var entry. It fails if a new Provider* constant is
// added to ProviderFamilies without its APIKeyEnvVars entry (or vice versa),
// which is the omission that let makora/together silently 502.
func TestEveryProviderHasFamilyAndEnvVar(t *testing.T) {
	all := providers.AllProviders()
	require.NotEmpty(t, all, "AllProviders returned no providers")

	for _, p := range all {
		assert.NotEqualf(t, providers.FamilyUnknown, providers.FamilyFor(p),
			"provider %q has no ProviderFamilies entry (FamilyUnknown)", p)
		assert.NotEmptyf(t, providers.APIKeyEnvVar(p),
			"provider %q has no APIKeyEnvVars entry", p)
	}
}

// TestValidateDispatchableRejectsUnknown asserts the boot guard flags a
// registered provider that has no family — the exact condition that would
// otherwise fall through the dispatch switches to ErrProviderNotConfigured.
func TestValidateDispatchableRejectsUnknown(t *testing.T) {
	// All real providers pass.
	require.NoError(t, providers.ValidateDispatchable(providers.AllProviders()))

	// A registered-but-unmapped provider is rejected, and its name surfaces in
	// the error so the dev knows which one to add.
	err := providers.ValidateDispatchable([]string{providers.ProviderOpenAI, "brand-new-provider"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "brand-new-provider")
}

// TestFamilyForKnownProviders pins the expected family of each provider so a
// mis-assignment (e.g. flipping an OpenAI-compat provider to Anthropic) fails.
func TestFamilyForKnownProviders(t *testing.T) {
	cases := map[string]providers.TranslationFamily{
		providers.ProviderAnthropic:  providers.FamilyAnthropic,
		providers.ProviderOpenAI:     providers.FamilyOpenAICompat,
		providers.ProviderGoogle:     providers.FamilyGemini,
		providers.ProviderOpenRouter: providers.FamilyOpenAICompat,
		providers.ProviderFireworks:  providers.FamilyOpenAICompat,
		providers.ProviderDeepInfra:  providers.FamilyOpenAICompat,
		providers.ProviderBedrock:    providers.FamilyOpenAICompat,
		providers.ProviderMakora:     providers.FamilyOpenAICompat,
		providers.ProviderTogether:   providers.FamilyOpenAICompat,
	}
	for p, want := range cases {
		assert.Equalf(t, want, providers.FamilyFor(p), "family for %q", p)
		assert.Equalf(t, want == providers.FamilyOpenAICompat, providers.IsOpenAICompat(p),
			"IsOpenAICompat for %q", p)
	}
}

// TestAllProvidersSorted asserts AllProviders returns a deterministic sorted
// slice (relied on for stable dashboard display order).
func TestAllProvidersSorted(t *testing.T) {
	all := providers.AllProviders()
	for i := 1; i < len(all); i++ {
		assert.LessOrEqualf(t, all[i-1], all[i], "AllProviders not sorted at index %d", i)
	}
}

// TestFamilyForUnknownIsZero asserts an unmapped provider name reports
// FamilyUnknown (the request-path backstop and boot-guard both rely on this).
func TestFamilyForUnknownIsZero(t *testing.T) {
	assert.Equal(t, providers.FamilyUnknown, providers.FamilyFor("nope-not-a-provider"))
}
