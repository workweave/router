package proxy

import (
	"context"
	"net/http"
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
)

// TestEnabledProvidersForRequest_PassthroughIsSurfaceScoped guards the fix for
// PR #159's cross-provider credential-leak review: a passthrough-eligible
// provider must join the eligible set only when the inbound surface matches
// its own. Without this, an Anthropic-surface request could route to OpenAI
// in a passthrough deployment and forward the inbound `x-api-key` (an
// Anthropic token) to api.openai.com.
func TestEnabledProvidersForRequest_PassthroughIsSurfaceScoped(t *testing.T) {
	s := &Service{
		// Mimic selfhosted with no env keys, both passthrough-eligible.
		providers: map[string]providers.Client{
			providers.ProviderAnthropic: nil,
			providers.ProviderOpenAI:    nil,
		},
		deploymentKeyedProviders: map[string]struct{}{},
		passthroughEligibleProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		},
	}

	t.Run("anthropic surface enables anthropic only", func(t *testing.T) {
		got := s.enabledProvidersForRequest(context.Background(), providers.ProviderAnthropic, http.Header{})
		assert.Contains(t, got, providers.ProviderAnthropic)
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"OpenAI must not be eligible on an Anthropic-surface request — the OpenAI client would forward the inbound x-api-key to api.openai.com")
	})

	t.Run("openai surface enables openai only", func(t *testing.T) {
		got := s.enabledProvidersForRequest(context.Background(), providers.ProviderOpenAI, http.Header{})
		assert.Contains(t, got, providers.ProviderOpenAI)
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"Anthropic must not be eligible on an OpenAI-surface request — the Anthropic client would forward the inbound Authorization Bearer to api.anthropic.com")
	})

	t.Run("empty surface enables neither passthrough provider", func(t *testing.T) {
		// surfaceProvider="" is the admin/passthrough-introspection path.
		got := s.enabledProvidersForRequest(context.Background(), "", http.Header{})
		assert.NotContains(t, got, providers.ProviderAnthropic)
		assert.NotContains(t, got, providers.ProviderOpenAI)
	})
}

// TestEnabledProvidersForRequest_DeploymentKeyedStillCrossSurface confirms
// that env-keyed providers stay eligible regardless of surface (their keys
// are trusted and don't depend on inbound headers).
func TestEnabledProvidersForRequest_DeploymentKeyedStillCrossSurface(t *testing.T) {
	s := &Service{
		providers: map[string]providers.Client{
			providers.ProviderAnthropic: nil,
			providers.ProviderOpenAI:    nil,
		},
		deploymentKeyedProviders: map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		},
		passthroughEligibleProviders: map[string]struct{}{},
	}

	got := s.enabledProvidersForRequest(context.Background(), providers.ProviderAnthropic, http.Header{})
	assert.Contains(t, got, providers.ProviderAnthropic)
	assert.Contains(t, got, providers.ProviderOpenAI,
		"env-keyed providers must remain eligible cross-surface; only passthrough is surface-scoped")
}
