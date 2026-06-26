package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	bypassSubToken      = "sk-ant-oat01-subscription-token"
	bypassRequestedMdl  = "claude-sonnet-4-6"
	bypassScorerPickMdl = "claude-haiku-4-5"
)

// bypassFixture builds a service whose fake scorer would route to
// bypassScorerPickMdl, with the subscription usage observer wired and a fake
// Anthropic provider that returns a minimal valid Messages response so a routed
// turn completes. seedUtil >= 0 pre-records an observation at that utilization
// under the subscription token; seedUtil < 0 leaves the observer cold.
func bypassFixture(t *testing.T, seedUtil float64) (*proxy.Service, *fakeRouter, *fakeProvider) {
	t.Helper()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + bypassScorerPickMdl + `","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	if seedUtil >= 0 {
		obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
			Primary: usage.Window{UsedPercent: seedUtil, WindowMinutes: 300},
		})
	}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)
	return svc, fr, p
}

// bypassCtx returns a ctx carrying a Claude subscription token plus a
// per-installation usage-bypass config at the given threshold.
func bypassCtx(threshold float64) context.Context {
	ctx := context.WithValue(context.Background(), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken)
	return context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{
		Enabled:   true,
		Threshold: &threshold,
	})
}

func bypassRequest(t *testing.T) (*httptest.ResponseRecorder, *http.Request, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"` + bypassRequestedMdl + `","messages":[{"role":"user","content":"hi"}]}`)
	return rec, req, body
}

// TestUsageBypass_BelowThreshold_SkipsScorer is the core contract: while the
// caller's subscription utilization is below the installation threshold, the
// scorer must NOT run and the requested model (not the scorer's pick) is served.
func TestUsageBypass_BelowThreshold_SkipsScorer(t *testing.T) {
	svc, fr, p := bypassFixture(t, 0.20)
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "scorer must not run while subscription has headroom")
	require.Len(t, p.proxyBodies, 1, "request must be dispatched to Anthropic exactly once")
	assert.Contains(t, string(p.proxyBodies[0]), `"`+bypassRequestedMdl+`"`, "bypass must preserve the caller-requested model")
	assert.Equal(t, "usage_bypass", rec.Header().Get("x-router-decision"))
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get("x-router-model"))
}

// TestUsageBypass_AtThreshold_EngagesRouting is the counterpart: once observed
// utilization crosses the threshold, the scorer runs and substitutes its pick.
func TestUsageBypass_AtThreshold_EngagesRouting(t *testing.T) {
	svc, fr, _ := bypassFixture(t, 0.90)
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "scorer must run once utilization crosses threshold")
	assert.Equal(t, bypassScorerPickMdl, rec.Header().Get("x-router-model"), "scorer's pick replaces the requested model")
}

// TestUsageBypass_ColdStart_Bypasses: with the gate on and a subscription
// present but no observation yet, the first turn serves on the subscription so
// its response primes the observer (mirrors the subsidy bootstrap).
func TestUsageBypass_ColdStart_Bypasses(t *testing.T) {
	svc, fr, _ := bypassFixture(t, -1) // observer left cold
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "cold start must bypass so the first turn primes the observer")
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get("x-router-model"))
}

// TestUsageBypass_GateDisabled_EngagesRouting: with no per-installation config
// on ctx the gate is off, so routing runs even with headroom.
func TestUsageBypass_GateDisabled_EngagesRouting(t *testing.T) {
	svc, fr, _ := bypassFixture(t, 0.20)
	rec, req, body := bypassRequest(t)
	// Subscription present, but the installation never enabled the gate.
	ctx := context.WithValue(context.Background(), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken)

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "scorer must run when the installation hasn't enabled the gate")
}

// TestUsageBypass_NoSubscription_EngagesRouting: the gate is on but the request
// carries no subscription credential, so there's nothing to pass through onto —
// routing runs.
func TestUsageBypass_NoSubscription_EngagesRouting(t *testing.T) {
	svc, fr, _ := bypassFixture(t, 0.20)
	rec, req, body := bypassRequest(t)
	threshold := 0.80
	ctx := context.WithValue(context.Background(), proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{
		Enabled:   true,
		Threshold: &threshold,
	})

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "no subscription credential means nothing to bypass onto — scorer must run")
}

// TestUsageBypass_ExcludedModel_EngagesRouting: a model on the installation's
// deny list must force routing even under threshold, so the bypass can't serve
// a policy-blocked model.
func TestUsageBypass_ExcludedModel_EngagesRouting(t *testing.T) {
	svc, fr, _ := bypassFixture(t, 0.20)
	svc = svc.WithExcludedModelsOverride([]string{bypassRequestedMdl})
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "an excluded requested model must force routing even under threshold")
}

// TestUsageBypass_ToolResult_BeatsStalePin: a session that previously routed
// (leaving a pin) and is now under threshold must bypass CONSISTENTLY. A
// tool_result continuation must serve the requested model via the bypass, not
// short-circuit to the stale pinned model through the tool-result sticky —
// otherwise the continuation hits a different model than the tool_use turn.
func TestUsageBypass_ToolResult_BeatsStalePin(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      providers.ProviderAnthropic,
		Model:         bypassScorerPickMdl, // stale pin from a prior routed stretch
		Reason:        "cluster:v0.2",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300}})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}}, nil, false, nil, store, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken)
	threshold := 0.80
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{Enabled: true, Threshold: &threshold})

	toolResultBody := []byte(`{"model":"` + bypassRequestedMdl + `","messages":[` +
		`{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	require.NoError(t, svc.ProxyMessages(ctx, toolResultBody, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "bypass must preempt the tool-result sticky, not run the scorer")
	assert.Equal(t, "usage_bypass", rec.Header().Get("x-router-decision"))
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get("x-router-model"), "continuation must serve the requested model, not the stale pin")
}

// TestUsageBypass_WorksWithoutSubsidyDiscount: the gate must engage when the
// observer is wired via WithUsageObserver alone (ROUTER_SUBSCRIPTION_AWARE_ROUTING
// off) — the bypass is opt-in per installation and must not depend on the
// separate subscription-aware cost-discount flag.
func TestUsageBypass_WorksWithoutSubsidyDiscount(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300}})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithUsageObserver(obs) // observer only — subsidy discount NOT enabled

	rec, req, body := bypassRequest(t)
	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "bypass must engage on observer alone, without the subsidy flag")
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get("x-router-model"))
}

// TestUsageBypass_ExcludedProvider_EngagesRouting: when the installation has
// excluded the Anthropic provider (e.g. data-residency policy), the bypass must
// not dispatch to Anthropic even under threshold — routing runs so the
// exclusion is honored.
func TestUsageBypass_ExcludedProvider_EngagesRouting(t *testing.T) {
	svc, fr, _ := bypassFixture(t, 0.20)
	rec, req, body := bypassRequest(t)
	ctx := context.WithValue(bypassCtx(0.80), proxy.InstallationExcludedProvidersContextKey{}, []string{providers.ProviderAnthropic})

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "an excluded Anthropic provider must force routing even under threshold")
}

// TestSubscriptionExhausted_ServesOnDeploymentKey is the customer-reported fix:
// once the caller's Claude subscription has bound its plan window (the observer
// records 100% utilization), a routed Anthropic turn must NOT keep injecting the
// spent OAuth token (which would 429 until reset). With a deployment Anthropic
// key wired, the turn drops the subscription and serves on that key instead, so
// the customer keeps working through the limit instead of hard-failing.
func TestSubscriptionExhausted_ServesOnDeploymentKey(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + bypassScorerPickMdl + `","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}}
	// Observer seeded EXHAUSTED on the weekly window.
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0).
		WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}})

	rec, req, body := bypassRequest(t)
	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	require.Len(t, p.proxyCreds, 1, "the turn must be dispatched once")
	creds := p.proxyCreds[0]
	if creds != nil {
		assert.False(t, creds.OAuth,
			"an exhausted subscription must not be forwarded — the turn serves on the deployment key")
	}
	// nil creds is also correct: no credential set means the Anthropic client
	// falls back to its own deployment key. Either way the spent token is gone.
}

// TestSubscriptionExhausted_NoDeploymentKey_KeepsSubscription guards the
// safety rail: with no deployment / BYOK Anthropic key to fall through to,
// dropping the subscription would leave the turn with no credential (a 400,
// worse than the 429). So the subscription is kept even when exhausted.
func TestSubscriptionExhausted_NoDeploymentKey_KeepsSubscription(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"` + bypassScorerPickMdl + `","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080},
	})
	// No WithDeploymentKeyedProviders — passthrough-only Anthropic.
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec, req, body := bypassRequest(t)
	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	require.Len(t, p.proxyCreds, 1)
	creds := p.proxyCreds[0]
	require.NotNil(t, creds, "with no fallback key the subscription must still be used")
	assert.True(t, creds.OAuth,
		"no deployment/BYOK key to fall through to — keep the subscription rather than 400")
}

// TestUsageBypass_ExhaustedDisengages_EvenAboveThreshold guards the failover
// hand-off: if an installation sets its threshold above exhaustedFraction, the
// gate must still disengage once the subscription is spent so the turn takes the
// routed path (where the exhaustion failover serves it on the Weave key) rather
// than bypassing onto a token that will 429.
func TestUsageBypass_ExhaustedDisengages_EvenAboveThreshold(t *testing.T) {
	// util 0.999 (exhausted) but BELOW a 1.0 threshold: the old `util < threshold`
	// check alone would keep the gate engaged and bypass onto the spent token.
	svc, fr, _ := bypassFixture(t, 0.999)
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(1.0), body, rec, req))

	assert.Equal(t, 1, fr.routeCalls,
		"an exhausted subscription must disengage the bypass so routing (and the failover) runs")
}
