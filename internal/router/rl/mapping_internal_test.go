package rl

import (
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/router/catalog"

	"github.com/stretchr/testify/assert"
)

// expectedRosterPrefix pins the vendor prefix rosterIDFor applies for a given
// provider's primary binding, keyed off the roster's OpenRouter-style slug
// convention (see rosterAliases doc comment). This map is deliberately
// explicit rather than derived from providers.ProviderFamilies so a new
// Provider* constant forces a decision here (finding [113]: provider
// dispatch drift guard) instead of silently falling through to the bare
// model-ID default.
var expectedRosterPrefix = map[string]string{
	providers.ProviderAnthropic: "anthropic/",
	providers.ProviderOpenAI:    "openai/",
	providers.ProviderGoogle:    "google/",
}

// defaultRosterPrefixProviders are providers intentionally left off
// expectedRosterPrefix because rosterIDFor's bare-model-ID fallback (no
// prefix) is correct for them — either the model ID is already slash-form
// (OpenAI-compat upstreams dispatched via catalog models with slash IDs) or
// the RL policy roster doesn't need to distinguish them.
var defaultRosterPrefixProviders = map[string]struct{}{
	providers.ProviderOpenRouter: {},
	providers.ProviderFireworks:  {},
	providers.ProviderDeepInfra:  {},
	providers.ProviderBedrock:    {},
	providers.ProviderMakora:     {},
	providers.ProviderTogether:   {},
}

// TestRosterIDForCoversEveryProvider guards against a new Provider* constant
// going unreviewed by rosterIDFor's per-provider switch in mapping.go. Every
// provider in providers.AllProviders() must be accounted for in exactly one
// of expectedRosterPrefix or defaultRosterPrefixProviders.
func TestRosterIDForCoversEveryProvider(t *testing.T) {
	for _, p := range providers.AllProviders() {
		_, explicit := expectedRosterPrefix[p]
		_, defaulted := defaultRosterPrefixProviders[p]
		assert.Truef(t, explicit || defaulted,
			"provider %q not accounted for in expectedRosterPrefix or defaultRosterPrefixProviders — "+
				"review internal/router/rl/mapping.go's rosterIDFor and internal/providers/CLAUDE.md's onboarding recipe step 5", p)
		assert.Falsef(t, explicit && defaulted,
			"provider %q is listed in both expectedRosterPrefix and defaultRosterPrefixProviders", p)
	}
}

// TestRosterIDForMatchesActualBehavior asserts rosterIDFor actually applies
// the pinned vendor prefix (or lack thereof) for every known provider, using
// a bare (non-slash, non-aliased) model ID per provider so the vendor-prefix
// branch of the switch is exercised rather than the alias or slash-form
// short-circuits.
func TestRosterIDForMatchesActualBehavior(t *testing.T) {
	const bareModelID = "some-bare-model-id"

	for _, p := range providers.AllProviders() {
		t.Run(p, func(t *testing.T) {
			model := catalog.Model{
				ID:        bareModelID,
				Providers: []catalog.ProviderBinding{{Provider: p}},
			}
			got := rosterIDFor(model)

			if prefix, ok := expectedRosterPrefix[p]; ok {
				assert.Equal(t, prefix+bareModelID, got)
				return
			}
			assert.Equal(t, bareModelID, got, "provider %q expected to fall through to the bare model ID", p)
		})
	}
}
