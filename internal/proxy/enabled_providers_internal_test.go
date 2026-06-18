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

// TestEnabledProvidersForRequest_ExcludedProvidersSubtracted confirms the
// per-installation provider exclusion list removes providers from the
// eligible set even when their credentials are wired — the single seam
// through which the scorer, hard pins, session pins, and tier clamp all
// inherit the exclusion.
func TestEnabledProvidersForRequest_ExcludedProvidersSubtracted(t *testing.T) {
	makeService := func() *Service {
		return &Service{
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
	}

	t.Run("installation exclusion removes a deployment-keyed provider", func(t *testing.T) {
		s := makeService()
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{providers.ProviderOpenAI})
		got := s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, http.Header{})
		assert.Contains(t, got, providers.ProviderAnthropic)
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"an excluded provider must be dropped even though its deployment key is wired")
	})

	t.Run("env override replaces the installation list", func(t *testing.T) {
		s := makeService().WithExcludedProvidersOverride([]string{providers.ProviderAnthropic})
		ctx := context.WithValue(context.Background(), InstallationExcludedProvidersContextKey{}, []string{providers.ProviderOpenAI})
		got := s.enabledProvidersForRequest(ctx, providers.ProviderAnthropic, http.Header{})
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"the deployment-wide override list must be enforced")
		assert.Contains(t, got, providers.ProviderOpenAI,
			"the override REPLACES the installation list rather than merging with it")
	})

	t.Run("no exclusions leaves the set untouched", func(t *testing.T) {
		s := makeService()
		got := s.enabledProvidersForRequest(context.Background(), providers.ProviderAnthropic, http.Header{})
		assert.Contains(t, got, providers.ProviderAnthropic)
		assert.Contains(t, got, providers.ProviderOpenAI)
	})
}

// TestEnabledProvidersForRequest_SubscriptionEnrollsAnthropic guards the
// managed-mode primary path: a router-keyed (installation set) byokOnly request
// carrying only the dedicated subscription header — no BYOK, no deployment key —
// must enroll Anthropic so the scorer can pick a Claude model. Without it the
// enabled set is empty and the scorer fails with ErrNoEligibleProvider before
// any Claude turn runs.
func TestEnabledProvidersForRequest_SubscriptionEnrollsAnthropic(t *testing.T) {
	makeService := func() *Service {
		return &Service{
			byokOnly: true,
			providers: map[string]providers.Client{
				providers.ProviderAnthropic: nil,
				providers.ProviderOpenAI:    nil,
			},
			deploymentKeyedProviders:     map[string]struct{}{},
			passthroughEligibleProviders: map[string]struct{}{},
		}
	}
	routerKeyed := func() context.Context {
		return context.WithValue(context.Background(), InstallationIDContextKey{}, testInstallationID)
	}

	t.Run("subscription header enrolls anthropic on a router-keyed byok-only request", func(t *testing.T) {
		ctx := context.WithValue(routerKeyed(), AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")
		got := makeService().enabledProvidersForRequest(ctx, providers.ProviderAnthropic, http.Header{})
		assert.Contains(t, got, providers.ProviderAnthropic,
			"a subscription token must make Anthropic eligible so the scorer can route a Claude turn to it")
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"the subscription token is Anthropic-only and must never enroll another upstream")
	})

	t.Run("inbound Authorization subscription bearer enrolls anthropic on a router-keyed request", func(t *testing.T) {
		// The managed Claude Code path: router key in X-Weave-Router-Key
		// (installation set), the subscription OAuth token left in Authorization.
		// Anthropic must be enrolled off the inbound bearer so the scorer can
		// route a Claude turn the subscription will pay for.
		headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderAnthropic, headers)
		assert.Contains(t, got, providers.ProviderAnthropic,
			"the inbound subscription bearer (CC-through-router) must enroll Anthropic even when router-keyed")
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"the subscription token is Anthropic-only and must never enroll another upstream")
	})

	t.Run("inbound API-key bearer does NOT enroll anthropic on a router-keyed request", func(t *testing.T) {
		// Only the sk-ant-oat OAuth subset enrolls off the inbound bearer; a real
		// client API key must not, mirroring resolveAndInjectCredentials and
		// preserving the cross-provider-leak guard.
		headers := http.Header{"Authorization": []string{"Bearer sk-ant-api-real-client-key"}}
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderAnthropic, headers)
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"a general inbound API key must not enroll Anthropic on the router-key path")
	})

	t.Run("no header leaves the set empty, proving the enrollment is load-bearing", func(t *testing.T) {
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderAnthropic, http.Header{})
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"without a subscription token a byok-only router-keyed request with no BYOK enrolls nothing")
		assert.Empty(t, got)
	})

	t.Run("an excluded Anthropic still trumps the subscription enrollment", func(t *testing.T) {
		ctx := context.WithValue(routerKeyed(), AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")
		ctx = context.WithValue(ctx, InstallationExcludedProvidersContextKey{}, []string{providers.ProviderAnthropic})
		got := makeService().enabledProvidersForRequest(ctx, providers.ProviderAnthropic, http.Header{})
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"a provider exclusion must subtract Anthropic even when a subscription token is present")
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
