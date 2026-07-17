package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/billing"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func oaiBypassBody() []byte {
	return []byte(`{"model":"` + bypassRequestedMdl + `","messages":[{"role":"user","content":"hi"}]}`)
}

// TestProxyOpenAI_UsageBypass_BelowThreshold_SkipsScorer: gate on + headroom skips scorer on OpenAI wire.
func TestProxyOpenAI_UsageBypass_BelowThreshold_SkipsScorer(t *testing.T) {
	svc, fr, p := bypassFixture(t, 0.20)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	require.NoError(t, svc.ProxyOpenAIChatCompletion(bypassCtx(0.80), oaiBypassBody(), rec, req))

	assert.Equal(t, 0, fr.routeCalls, "scorer must not run while subscription has headroom")
	require.Len(t, p.proxyBodies, 1)
	assert.Contains(t, string(p.proxyBodies[0]), `"`+bypassRequestedMdl+`"`)
	assert.Equal(t, "usage_bypass", rec.Header().Get(proxy.HeaderRouterDecision))
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Contains(t, rec.Body.String(), `"object":"chat.completion"`)
}

// TestProxyOpenAI_UsageBypass_WeeklyLimit_FallsBackToRoutedDispatch: bypass 429 must reroute via routeFor.
func TestProxyOpenAI_UsageBypass_WeeklyLimit_FallsBackToRoutedDispatch(t *testing.T) {
	bypassResp := &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Headers: http.Header{
			"anthropic-ratelimit-unified-weekly-limit":     []string{"100000"},
			"anthropic-ratelimit-unified-weekly-reset":     []string{"2025-12-31T00:00:00Z"},
			"anthropic-ratelimit-unified-weekly-remaining": []string{"0"},
		},
		Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit exceeded"}}`),
	}
	routedResp := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + bypassScorerPickMdl + `","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	inner := &fakeProvider{proxyResponse: routedResp}
	wrappedP := &swapErrProvider{first: bypassResp, second: nil, inner: inner}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl, Reason: "cluster:v0.2"}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: wrappedP}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	ctx := bypassCtx(0.80)
	ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, "org-oai-bypass-reroute")
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, uuid.New().String())

	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, oaiBypassBody(), rec, req))

	assert.Equal(t, 1, fr.routeCalls, "scorer must run once on the reroute after bypass 429")
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code, "the 429 must not be flushed to the client")
	assert.Equal(t, bypassScorerPickMdl, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, "cluster:v0.2", rec.Header().Get(proxy.HeaderRouterDecision))
	assert.Contains(t, rec.Body.String(), `"object":"chat.completion"`)
}

// TestSubscriptionOnly_OpenAI_BypassRetryable_Refuses402: subscription-only refuses retryable bypass failure.
func TestSubscriptionOnly_OpenAI_BypassRetryable_Refuses402(t *testing.T) {
	bypassResp := &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit exceeded"}}`),
	}
	p := &fakeProvider{proxyErr: bypassResp}
	wrappedP := &swapErrProvider{first: bypassResp, second: nil, inner: p}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl, Reason: "cluster:v0.2"}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: wrappedP}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	err := svc.ProxyOpenAIChatCompletion(billing.WithSubscriptionOnly(bypassCtx(0.80)), oaiBypassBody(), rec, req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable))
	assert.Equal(t, 0, fr.routeCalls)
	assert.Equal(t, 1, wrappedP.calls)
}
