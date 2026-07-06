package translate_test

import (
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sessionAffinityMechanism is which knob applySessionAffinity uses to convey
// SessionAffinity to a given provider.
type sessionAffinityMechanism int

const (
	mechanismGenericHeader sessionAffinityMechanism = iota // x-session-affinity
	mechanismSessionIDHeader
	mechanismPromptCacheKeyBody
	mechanismNone
)

// expectedSessionAffinityMechanism pins the affinity mechanism for every
// OpenAI-compat provider applySessionAffinity currently knows about (finding
// [113]: provider dispatch drift guard). This map is deliberately explicit
// (not derived from providers.ProviderFamilies) so that adding a new
// Provider* constant that's OpenAI-compat family forces a decision here
// instead of silently inheriting whatever the switch's default happens to
// do. If this test fails after adding a provider, either add an explicit
// mechanism here (if the provider needs bespoke affinity handling, per
// internal/providers/CLAUDE.md's onboarding recipe) or add it to
// defaultMechanismProviders below to affirm the generic header is correct.
var expectedSessionAffinityMechanism = map[string]sessionAffinityMechanism{
	providers.ProviderOpenRouter: mechanismSessionIDHeader,
	providers.ProviderOpenAI:     mechanismPromptCacheKeyBody,
	providers.ProviderBedrock:    mechanismNone,
}

// defaultMechanismProviders are OpenAI-compat providers intentionally left
// off expectedSessionAffinityMechanism because the generic
// x-session-affinity header default is correct for them.
var defaultMechanismProviders = map[string]struct{}{
	providers.ProviderFireworks: {},
	providers.ProviderDeepInfra: {},
	providers.ProviderMakora:    {},
	providers.ProviderTogether:  {},
}

// TestSessionAffinityCoversEveryOpenAICompatProvider guards against a new
// OpenAI-compat provider (or a re-family'd existing one) going unreviewed by
// applySessionAffinity's per-provider switch in emit_openai.go. Every
// provider in providers.AllProviders() that speaks the OpenAI-compat family
// must be accounted for in exactly one of expectedSessionAffinityMechanism or
// defaultMechanismProviders.
func TestSessionAffinityCoversEveryOpenAICompatProvider(t *testing.T) {
	for _, p := range providers.AllProviders() {
		if !providers.IsOpenAICompat(p) {
			continue
		}
		_, explicit := expectedSessionAffinityMechanism[p]
		_, defaulted := defaultMechanismProviders[p]
		require.Truef(t, explicit || defaulted,
			"provider %q is OpenAI-compat but not accounted for in expectedSessionAffinityMechanism or defaultMechanismProviders — "+
				"review internal/translate/emit_openai.go's applySessionAffinity and internal/providers/CLAUDE.md's onboarding recipe step 5", p)
		require.Falsef(t, explicit && defaulted,
			"provider %q is listed in both expectedSessionAffinityMechanism and defaultMechanismProviders", p)
	}

	for p := range expectedSessionAffinityMechanism {
		assert.Truef(t, providers.IsOpenAICompat(p), "expectedSessionAffinityMechanism has stale non-OpenAI-compat provider %q", p)
	}
	for p := range defaultMechanismProviders {
		assert.Truef(t, providers.IsOpenAICompat(p), "defaultMechanismProviders has stale non-OpenAI-compat provider %q", p)
	}
}

// TestSessionAffinityMechanismMatchesActualBehavior asserts applySessionAffinity
// (exercised via PrepareOpenAI) actually produces the mechanism pinned above,
// for every known OpenAI-compat provider.
func TestSessionAffinityMechanismMatchesActualBehavior(t *testing.T) {
	const affinityKey = "0123456789abcdef0123456789abcdef"

	for _, p := range providers.AllProviders() {
		if !providers.IsOpenAICompat(p) {
			continue
		}
		mechanism, ok := expectedSessionAffinityMechanism[p]
		if !ok {
			mechanism = mechanismGenericHeader
		}

		t.Run(p, func(t *testing.T) {
			env, err := translate.ParseAnthropic(anthropicSrc())
			require.NoError(t, err)

			out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
				TargetModel:     "some/model",
				TargetProvider:  p,
				SessionAffinity: affinityKey,
			})
			require.NoError(t, err)

			gotSessionID := out.Headers.Get("x-session-id")
			gotGenericHeader := out.Headers.Get("x-session-affinity")
			_, gotBody := promptCacheKey(t, out.Body)

			switch mechanism {
			case mechanismSessionIDHeader:
				assert.Equal(t, affinityKey, gotSessionID)
				assert.Empty(t, gotGenericHeader)
				assert.False(t, gotBody)
			case mechanismPromptCacheKeyBody:
				assert.Empty(t, gotSessionID)
				assert.Empty(t, gotGenericHeader)
				assert.True(t, gotBody)
			case mechanismNone:
				assert.Empty(t, gotSessionID)
				assert.Empty(t, gotGenericHeader)
				assert.False(t, gotBody)
			case mechanismGenericHeader:
				assert.Empty(t, gotSessionID)
				assert.Equal(t, affinityKey, gotGenericHeader)
				assert.False(t, gotBody)
			}
		})
	}
}
