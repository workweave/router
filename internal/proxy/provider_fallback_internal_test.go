package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/httputil"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedClient is a stub providers.Client where each Proxy call returns the
// next entry in the err slice. recordedDecisions captures the decisions the
// helper actually dispatched against, in order, so the test can assert which
// provider got called when.
type scriptedClient struct {
	name             string
	errs             []error
	stampFirstByte   bool
	calls            atomic.Int32
	gotDecisions     []router.Decision
	gotPreparedModel []string
}

func (c *scriptedClient) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	idx := int(c.calls.Add(1) - 1)
	c.gotDecisions = append(c.gotDecisions, decision)
	c.gotPreparedModel = append(c.gotPreparedModel, decision.Model)
	if c.stampFirstByte {
		otel.TimingFrom(ctx).StampUpstreamFirstByte()
	}
	if idx >= len(c.errs) {
		return nil
	}
	return c.errs[idx]
}

func (c *scriptedClient) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return nil
}

// envelopeFromAnthropic builds a trivial RequestEnvelope so PrepareOpenAI works.
func envelopeFromAnthropic(t *testing.T) *translate.RequestEnvelope {
	t.Helper()
	env, err := translate.ParseAnthropic([]byte(`{
		"model": "claude-opus-4-7",
		"max_tokens": 64,
		"messages": [{"role":"user","content":"hi"}]
	}`))
	require.NoError(t, err)
	return env
}

func newFallbackService(t *testing.T, clients map[string]providers.Client) *Service {
	t.Helper()
	return &Service{
		providers: clients,
		// Other fields default to zero; proxyWithFallback only reads providers.
	}
}

func TestProxyWithFallback_HappyPathNoFallback(t *testing.T) {
	clients := map[string]providers.Client{
		providers.ProviderBedrock: &scriptedClient{name: "bedrock"},
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: decision.Provider}, nil, w, r)

	require.NoError(t, err)
	assert.False(t, outcome.Attempted)
	assert.Equal(t, providers.ProviderBedrock, decision.Provider, "decision must remain on the original provider")
}

func TestProxyWithFallback_RetriesOnIdleTimeoutBeforeFirstByte(t *testing.T) {
	// qwen3-235b-a22b-2507 has bindings [bedrock, openrouter]; the bedrock
	// client returns an idle-timeout, the openrouter client succeeds.
	first := &scriptedClient{name: "bedrock", errs: []error{httputil.ErrUpstreamIdleTimeout}}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, nil, w, r)

	require.NoError(t, err)
	assert.True(t, outcome.Attempted)
	assert.Equal(t, providers.ProviderBedrock, outcome.FromProvider)
	assert.Equal(t, providers.ProviderOpenRouter, outcome.ToProvider)
	assert.Equal(t, "sse_idle", outcome.Reason)
	assert.ErrorIs(t, outcome.FirstErr, httputil.ErrUpstreamIdleTimeout)
	assert.Equal(t, int32(1), first.calls.Load())
	assert.Equal(t, int32(1), second.calls.Load())
	assert.Equal(t, providers.ProviderOpenRouter, decision.Provider, "decision must reflect the actually-served provider")
}

func TestProxyWithFallback_RetriesOnUpstream5xxBeforeFirstByte(t *testing.T) {
	first := &scriptedClient{name: "bedrock", errs: []error{&providers.UpstreamStatusError{Status: 503}}}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, nil, w, r)

	require.NoError(t, err)
	assert.True(t, outcome.Attempted)
	assert.Equal(t, "5xx", outcome.Reason)
}

func TestProxyWithFallback_NoRetryAfterFirstByte(t *testing.T) {
	// Even though the error is idle-timeout, the upstream produced bytes
	// before stalling — the translator has emitted deltas, so retrying
	// against a different provider would interleave two streams.
	first := &scriptedClient{
		name:           "bedrock",
		errs:           []error{httputil.ErrUpstreamIdleTimeout},
		stampFirstByte: true,
	}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, nil, w, r)

	require.Error(t, err)
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.False(t, outcome.Attempted, "retry must not fire once any upstream byte has reached the translator")
	assert.Equal(t, int32(0), second.calls.Load())
	assert.Equal(t, providers.ProviderBedrock, decision.Provider, "no retry → decision stays on original provider")
}

func TestProxyWithFallback_NoRetryOn4xx(t *testing.T) {
	first := &scriptedClient{name: "bedrock", errs: []error{&providers.UpstreamStatusError{Status: 429}}}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, nil, w, r)

	require.Error(t, err)
	assert.False(t, outcome.Attempted, "4xx (e.g. 429 rate-limit) is the caller's problem; do not retry against another provider")
	assert.Equal(t, int32(0), second.calls.Load())
}

func TestProxyWithFallback_NoBindingsToFallbackToReturnsOriginalError(t *testing.T) {
	// claude-opus-4-7 is anthropic-only — single binding, no fallback available.
	first := &scriptedClient{name: "anthropic", errs: []error{httputil.ErrUpstreamIdleTimeout}}
	clients := map[string]providers.Client{
		providers.ProviderAnthropic: first,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "claude-opus-4-7", Provider: providers.ProviderAnthropic}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderAnthropic}, nil, w, r)

	require.Error(t, err)
	assert.False(t, outcome.Attempted)
}

func TestProxyWithFallback_SkipsBindingsWithUnwiredProvider(t *testing.T) {
	// catalog has [bedrock, openrouter] for qwen3-235b; this deploy only
	// wired bedrock. With no openrouter client in the providers map, there
	// is no eligible fallback target.
	first := &scriptedClient{name: "bedrock", errs: []error{httputil.ErrUpstreamIdleTimeout}}
	clients := map[string]providers.Client{
		providers.ProviderBedrock: first,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, nil, w, r)

	require.Error(t, err)
	assert.False(t, outcome.Attempted)
}

func TestProxyWithFallback_RespectsEnabledProviders_BYOK(t *testing.T) {
	// BYOK request: customer only authorized the deploy to spend its
	// Bedrock credentials. After a Bedrock idle timeout the catalog still
	// lists OpenRouter as the next binding and the deploy has an OpenRouter
	// client wired, but the request never authorized that provider — silently
	// falling over would charge deployment credentials for a provider the
	// request did not opt into.
	first := &scriptedClient{name: "bedrock", errs: []error{httputil.ErrUpstreamIdleTimeout}}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	enabledProviders := map[string]struct{}{providers.ProviderBedrock: {}}
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, enabledProviders, w, r)

	require.Error(t, err)
	assert.ErrorIs(t, err, httputil.ErrUpstreamIdleTimeout)
	assert.False(t, outcome.Attempted, "OpenRouter is wired but not in the request's eligible set; fallback must not fire")
	assert.Equal(t, int32(0), second.calls.Load(), "openrouter must not be dispatched against")
	assert.Equal(t, providers.ProviderBedrock, decision.Provider, "no eligible fallback → decision stays on the original provider")
}

func TestProxyWithFallback_RespectsEnabledProviders_AllowsEligible(t *testing.T) {
	// Same setup but the request authorized both providers — fallback fires.
	first := &scriptedClient{name: "bedrock", errs: []error{httputil.ErrUpstreamIdleTimeout}}
	second := &scriptedClient{name: "openrouter"}
	clients := map[string]providers.Client{
		providers.ProviderBedrock:    first,
		providers.ProviderOpenRouter: second,
	}
	s := newFallbackService(t, clients)

	ctx, _ := otel.WithTiming(context.Background())
	decision := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: providers.ProviderBedrock}
	env := envelopeFromAnthropic(t)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	enabledProviders := map[string]struct{}{providers.ProviderBedrock: {}, providers.ProviderOpenRouter: {}}
	err, outcome := s.proxyWithFallback(ctx, &decision, env, translate.EmitOptions{TargetModel: decision.Model, TargetProvider: providers.ProviderBedrock}, enabledProviders, w, r)

	require.NoError(t, err)
	assert.True(t, outcome.Attempted)
	assert.Equal(t, providers.ProviderOpenRouter, decision.Provider)
}

func TestClassifyFallbackError(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantReason  string
		wantRetryOK bool
	}{
		{"idle", httputil.ErrUpstreamIdleTimeout, "sse_idle", true},
		{"500", &providers.UpstreamStatusError{Status: 500}, "5xx", true},
		{"503", &providers.UpstreamStatusError{Status: 503}, "5xx", true},
		{"429", &providers.UpstreamStatusError{Status: 429}, "", false},
		{"400", &providers.UpstreamStatusError{Status: 400}, "", false},
		{"nil", nil, "", false},
		{"unknown", errors.New("other"), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason, ok := classifyFallbackError(c.err)
			assert.Equal(t, c.wantRetryOK, ok)
			assert.Equal(t, c.wantReason, reason)
		})
	}
}
