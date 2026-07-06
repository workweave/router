package proxy

import (
	"context"
	"net/http"
	"testing"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
)

// TestEnabledProvidersForRequest_PassthroughIsSurfaceScoped guards PR #159's
// credential-leak fix: a passthrough-eligible provider is only eligible when
// the inbound surface matches its own, or e.g. an Anthropic x-api-key could
// leak to api.openai.com.
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
// per-installation exclusion list removes providers even when credentials
// are wired — the single seam scorer, pins, and tier clamp all inherit from.
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
// managed-mode path: a router-keyed byokOnly request carrying only the
// subscription header must enroll Anthropic, or the scorer fails with
// ErrNoEligibleProvider before any Claude turn runs.
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
		// Managed Claude Code path: router key in X-Weave-Router-Key, OAuth
		// token in Authorization — Anthropic must enroll off the inbound bearer.
		headers := http.Header{"Authorization": []string{"Bearer sk-ant-oat01-subscription-token"}}
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderAnthropic, headers)
		assert.Contains(t, got, providers.ProviderAnthropic,
			"the inbound subscription bearer (CC-through-router) must enroll Anthropic even when router-keyed")
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"the subscription token is Anthropic-only and must never enroll another upstream")
	})

	t.Run("inbound API-key bearer does NOT enroll anthropic on a router-keyed request", func(t *testing.T) {
		// Only the sk-ant-oat OAuth subset enrolls off the bearer; a real API key must not.
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

// TestEnabledProvidersForRequest_CodexSubscriptionEnrollsOpenAI mirrors the
// Anthropic subscription test for Codex: enrolling OpenAI requires BOTH the
// JWT and account-id.
func TestEnabledProvidersForRequest_CodexSubscriptionEnrollsOpenAI(t *testing.T) {
	const codexJWT = "eyJhbGciOiJSUzI1NiJ9.codex.sig"
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

	t.Run("dedicated Codex headers enroll openai only", func(t *testing.T) {
		ctx := context.WithValue(routerKeyed(), OpenAISubscriptionContextKey{}, codexJWT)
		ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-123")
		got := makeService().enabledProvidersForRequest(ctx, providers.ProviderOpenAI, http.Header{})
		assert.Contains(t, got, providers.ProviderOpenAI,
			"a Codex subscription must make OpenAI eligible so the scorer can route a Codex turn")
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"the Codex token is OpenAI-only and must never enroll another upstream")
	})

	t.Run("inbound Authorization Codex bearer enrolls openai on a router-keyed request", func(t *testing.T) {
		// Managed Codex CLI path: router key in X-Weave-Router-Key, JWT+account-id
		// in Authorization — OpenAI must enroll off the inbound bearer.
		headers := http.Header{
			"Authorization":      []string{"Bearer " + codexJWT},
			"Chatgpt-Account-Id": []string{"acct-123"},
		}
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderOpenAI, headers)
		assert.Contains(t, got, providers.ProviderOpenAI,
			"the inbound Codex subscription bearer (Codex-through-router) must enroll OpenAI even when router-keyed")
		assert.NotContains(t, got, providers.ProviderAnthropic,
			"the Codex token is OpenAI-only and must never enroll another upstream")
	})

	t.Run("inbound OpenAI API-key bearer does NOT enroll openai on a router-keyed request", func(t *testing.T) {
		// Only the Codex OAuth subset (JWT + account-id) enrolls off the inbound
		// bearer; a plain client API key with no account-id must not.
		headers := http.Header{"Authorization": []string{"Bearer sk-proj-real-client-key"}}
		got := makeService().enabledProvidersForRequest(routerKeyed(), providers.ProviderOpenAI, headers)
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"a general inbound OpenAI API key must not enroll OpenAI on the router-key path")
	})

	t.Run("token without account-id enrolls nothing (load-bearing)", func(t *testing.T) {
		ctx := context.WithValue(routerKeyed(), OpenAISubscriptionContextKey{}, codexJWT)
		got := makeService().enabledProvidersForRequest(ctx, providers.ProviderOpenAI, http.Header{})
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"without the ChatGPT-Account-ID the subscription is unusable, so OpenAI must not be enrolled")
		assert.Empty(t, got)
	})

	t.Run("an excluded OpenAI trumps the Codex enrollment", func(t *testing.T) {
		ctx := context.WithValue(routerKeyed(), OpenAISubscriptionContextKey{}, codexJWT)
		ctx = context.WithValue(ctx, OpenAIAccountIDContextKey{}, "acct-123")
		ctx = context.WithValue(ctx, InstallationExcludedProvidersContextKey{}, []string{providers.ProviderOpenAI})
		got := makeService().enabledProvidersForRequest(ctx, providers.ProviderOpenAI, http.Header{})
		assert.NotContains(t, got, providers.ProviderOpenAI,
			"a provider exclusion must subtract OpenAI even when a Codex subscription is present")
	})
}

// TestEnabledProvidersForRequest_DeploymentKeyedStillCrossSurface confirms
// env-keyed providers stay eligible regardless of surface.
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
