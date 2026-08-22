package proxy

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
)

func excludedModelsCtx(names ...string) context.Context {
	return context.WithValue(context.Background(), InstallationExcludedModelsContextKey{}, names)
}

// A summary ships the whole prior conversation upstream, so an excluded
// provider must be refused even though the summarizer is wired deployment-wide.
func TestGateSummarizerCall_RefusesExcludedProvider(t *testing.T) {
	s := &Service{}

	gate := s.gateSummarizerCall(excludedProvidersCtx(providers.ProviderAnthropic),
		providers.ProviderAnthropic, DefaultHandoverModel, http.Header{})

	assert.False(t, gate.Allowed)
	assert.Equal(t, summarizerSkipProviderExcluded, gate.SkipReason)
}

// Deployment-wide exclusions bind the summarizer too.
func TestGateSummarizerCall_RefusesDeploymentExcludedProvider(t *testing.T) {
	s := (&Service{}).WithExcludedProvidersOverride([]string{providers.ProviderAnthropic})

	gate := s.gateSummarizerCall(context.Background(),
		providers.ProviderAnthropic, DefaultHandoverModel, http.Header{})

	assert.False(t, gate.Allowed)
	assert.Equal(t, summarizerSkipProviderExcluded, gate.SkipReason)
}

func TestGateSummarizerCall_RefusesExcludedModel(t *testing.T) {
	s := &Service{}

	gate := s.gateSummarizerCall(excludedModelsCtx(DefaultHandoverModel),
		providers.ProviderAnthropic, DefaultHandoverModel, http.Header{})

	assert.False(t, gate.Allowed)
	assert.Equal(t, summarizerSkipModelExcluded, gate.SkipReason)
}

// An unrelated exclusion must not disable summarization.
func TestGateSummarizerCall_AllowsPermittedBinding(t *testing.T) {
	s := &Service{}

	gate := s.gateSummarizerCall(excludedProvidersCtx(providers.ProviderOpenAI),
		providers.ProviderAnthropic, DefaultHandoverModel, http.Header{})

	require.True(t, gate.Allowed)
	assert.Empty(t, gate.SkipReason)
	assert.Nil(t, gate.Creds, "no caller creds forwarded → run on the deployment key")
}

// Transient 529 strike-outs are evidence, not policy: they must not silently
// disable summarization for the rest of the session.
func TestGateSummarizerCall_IgnoresSessionStrikeOuts(t *testing.T) {
	s := &Service{}
	ctx := context.WithValue(context.Background(),
		SessionDisabledProvidersContextKey{}, []string{providers.ProviderAnthropic})

	gate := s.gateSummarizerCall(ctx, providers.ProviderAnthropic, DefaultHandoverModel, http.Header{})

	assert.True(t, gate.Allowed)
}

// The tenant boundary still applies: a client-keyed request with no matching
// forwarded credential must not spend the deployment key.
func TestGateSummarizerCall_RefusesTenantBoundaryCrossing(t *testing.T) {
	s := &Service{}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-customer-openai-key")

	gate := s.gateSummarizerCall(context.Background(), providers.ProviderAnthropic, DefaultHandoverModel, headers)

	assert.False(t, gate.Allowed)
	assert.Equal(t, summarizerSkipTenantBoundary, gate.SkipReason)
}

// The exclusion check runs before credential resolution: forwarding the
// caller's own key does not buy egress to a provider the operator excluded.
func TestGateSummarizerCall_ExclusionOutranksCallerCreds(t *testing.T) {
	s := &Service{}
	headers := http.Header{}
	headers.Set("x-api-key", "sk-ant-customer-byok-key")

	gate := s.gateSummarizerCall(excludedProvidersCtx(providers.ProviderAnthropic),
		providers.ProviderAnthropic, DefaultHandoverModel, headers)

	assert.False(t, gate.Allowed)
	assert.Equal(t, summarizerSkipProviderExcluded, gate.SkipReason)
	assert.Nil(t, gate.Creds)
}

// Compaction picks its own model, so the window-aware selector must skip an
// excluded one rather than hand it the history.
func TestSelectCompactionSummarizer_SkipsExcludedModel(t *testing.T) {
	s := &Service{}

	got := s.selectCompactionSummarizer(excludedModelsCtx(DefaultHandoverModel), 1_000)

	assert.Equal(t, largeWindowSummarizerModel, got, "cheap model excluded → next permitted window")

	got = s.selectCompactionSummarizer(
		excludedModelsCtx(DefaultHandoverModel, largeWindowSummarizerModel), 1_000)

	assert.Empty(t, got, "every summarizer model excluded → no summarization")
}

// The summarizer must report the provider it was actually built for, or
// credential resolution keys off the wrong one.
func TestProviderSummarizer_ReportsConfiguredProvider(t *testing.T) {
	s := NewProviderSummarizer(nil, providers.ProviderAnthropicGateway, "gateway-model", 0)

	assert.Equal(t, providers.ProviderAnthropicGateway, s.Provider())
	assert.Equal(t, "gateway-model", s.Model())

	assert.Equal(t, providers.ProviderAnthropic, NewProviderSummarizer(nil, "", "", 0).Provider())
}
