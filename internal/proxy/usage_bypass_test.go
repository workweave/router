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

func TestUsageBypass_CodexSubscriptionPreservesRequestedModel(t *testing.T) {
	const codexToken = "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ0ZXN0In0.signature"
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[]}`))
	}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(codexToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderOpenAI: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithUsageObserver(obs)
	threshold := 0.80
	ctx := context.WithValue(context.Background(), proxy.OpenAISubscriptionContextKey{}, codexToken)
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, "account-1")
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{
		Enabled:   true,
		Threshold: &threshold,
	})
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "strict pass-through must not call the scorer")
	require.Len(t, p.proxyBodies, 1)
	assert.Contains(t, string(p.proxyBodies[0]), `"model":"gpt-5.5"`)
	assert.Equal(t, "usage_bypass", rec.Header().Get("x-router-decision"))
	assert.Equal(t, "gpt-5.5", rec.Header().Get("x-router-model"))
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

// TestUsageBypass_InstallationExcludedModel_StillBypasses: installation
// excluded_models is a routing preference, not a hard block. A bypass-enabled
// caller must be served on their own subscription even for a policy-excluded model.
func TestUsageBypass_InstallationExcludedModel_StillBypasses(t *testing.T) {
	svc, fr, p := bypassFixture(t, 0.20)
	svc = svc.WithExcludedModelsOverride([]string{bypassRequestedMdl})
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(bypassCtx(0.80), body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "an installation-excluded model must still bypass — the exclusion is a routing preference, not a hard block")
	require.Len(t, p.proxyBodies, 1, "the requested model must be dispatched to the subscription exactly once")
	assert.Equal(t, bypassRequestedMdl, rec.Header().Get("x-router-model"), "bypass must serve the requested model, not a substituted one")
}

// TestUsageBypass_MaxedOutModel_EngagesRouting: the maxed-out guard writes
// the saturated model to SafetyExcludedModels so an auto-continue re-request
// falls through to the scorer, preventing bypass from reopening the max-output loop.
func TestUsageBypass_MaxedOutModel_EngagesRouting(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		Model:            bypassRequestedMdl, // the model the client keeps requesting
		LastServedModel:  bypassRequestedMdl, // saturated the output cap last turn
		Reason:           "cluster:v0.2",
		PinnedUntil:      time.Now().Add(30 * time.Minute),
		FirstPinnedAt:    time.Now().Add(-5 * time.Minute),
		LastOutputTokens: 8192, // >= prevTurnMaxedOutThreshold
		LastTurnEndedAt:  time.Now().Add(-10 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl, Reason: "fresh"}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300}})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}}, nil, false, nil, store, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken)
	threshold := 0.80
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{Enabled: true, Threshold: &threshold})
	rec, req, body := bypassRequest(t)

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 1, fr.routeCalls, "a maxed-out model must fall through to the scorer, not re-bypass and reopen the loop")
	require.NotNil(t, fr.capturedReq)
	assert.Contains(t, fr.capturedReq.SafetyExcludedModels, bypassRequestedMdl,
		"the maxed-out model must be in the safety-exclusion set so the bypass gate refuses it")
	assert.Equal(t, bypassScorerPickMdl, rec.Header().Get("x-router-model"), "scorer's pick replaces the saturated model")
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

// TestProxyMessages_BypassWeeklyLimit_FallsBackToRoutedDispatch is the
// end-to-end contract for the in-turn fall-through: a bypass attempt that
// returns a buffered 429 must NOT be flushed at the client. ProxyMessages must
// discard the bypass state, re-resolve via the normal routed path, and serve
// the turn on the scorer's pick (a non-Anthropic model in this fixture).
func TestProxyMessages_BypassWeeklyLimit_FallsBackToRoutedDispatch(t *testing.T) {
	// Bypass attempt returns a buffered 429 with weekly-limit headers. The
	// fakeProvider captures this as proxyErr; the routed fallback uses the same
	// provider (the fixture only wires Anthropic), so the second dispatch
	// succeeds with a 200.
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
	p := &fakeProvider{proxyErr: bypassResp, proxyResponse: routedResp}
	// The fakeProvider returns the SAME proxyErr on every dispatch. To make the
	// second (routed) dispatch succeed, swap proxyErr to nil after the first call.
	wrappedP := &swapErrProvider{first: bypassResp, second: nil, inner: p}

	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl, Reason: "cluster:v0.2"}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Seed under threshold so the bypass engages for the first attempt.
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{
		providers.ProviderAnthropic:  wrappedP,
		providers.ProviderOpenRouter: &fakeProvider{},
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0).
		WithTranslationCompatibilityMode(proxy.TranslationCompatibilityEnforce)

	rec, req, body := bypassRequest(t)
	body = []byte(`{"model":"` + bypassRequestedMdl + `","system":[{"type":"text","text":"cached instructions","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	const organizationID = "org-bypass-reroute"
	installationID := uuid.New()
	ctx := context.WithValue(bypassCtx(0.80), proxy.ExternalIDContextKey{}, organizationID)
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, installationID.String())
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 1, fr.routeCalls,
		"the scorer must run once on the reroute (zero on the bypass attempt, one after the 429)")
	require.NotNil(t, fr.capturedReq)
	assert.Equal(t, organizationID, fr.capturedReq.OrganizationID)
	assert.Equal(t, installationID.String(), fr.capturedReq.InstallationID)
	assert.True(t, fr.capturedReq.TranslationRequirements.PromptCacheControl)
	assert.Equal(t, map[string]struct{}{providers.ProviderAnthropic: {}}, fr.capturedReq.EnabledProviders,
		"reroute must retain translation-plan provider constraints")
	assert.NotEqual(t, http.StatusTooManyRequests, rec.Code,
		"the 429 must NOT be flushed — the client must not see the bypass failure")
	assert.Equal(t, bypassScorerPickMdl, rec.Header().Get("x-router-model"),
		"the routed fallback's model replaces the bypass requested model")
	assert.Equal(t, "cluster:v0.2", rec.Header().Get("x-router-decision"),
		"the routed path's decision reason replaces the usage_bypass marker — the 429 must not be the last word")
}

// bypassStreamResponse writes a minimal valid Anthropic SSE stream so the
// subscription-only warning marker (which only injects on a streaming response)
// has a stream to prepend to.
func bypassStreamResponse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_up\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"" + bypassRequestedMdl + "\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
	_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
	_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
	_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"))
	_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
}

// TestSubscriptionOnly_ServesOnSubscription_EvenAboveThreshold: with the org
// past the overdraft floor (subscription-only mode), the usage-bypass gate must
// stay engaged even when observed utilization is ABOVE the installation
// threshold — paid failover is disabled, so conserving quota by routing
// elsewhere isn't an option. The turn serves on the subscription (scorer never
// runs) and the customer sees a depleted-credits warning prepended to the reply.
func TestSubscriptionOnly_ServesOnSubscription_EvenAboveThreshold(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Seeded ABOVE the 0.80 threshold: without subscription-only mode this would
	// disengage the bypass and run the scorer (see TestUsageBypass_AtThreshold).
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.90, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec, req, body := bypassRequest(t)
	ctx := billing.WithSubscriptionOnly(bypassCtx(0.80))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Equal(t, 0, fr.routeCalls, "subscription-only mode must keep the bypass engaged, not run the scorer")
	require.Len(t, p.proxyBodies, 1, "the turn must serve on the subscription exactly once")
	assert.Contains(t, string(p.proxyBodies[0]), `"`+bypassRequestedMdl+`"`, "bypass must preserve the caller-requested model")
	assert.Contains(t, rec.Body.String(), "credits are depleted", "the customer must see the depleted-credits warning")
	assert.Contains(t, rec.Body.String(), "router-credits", "the warning must surface the top-up CTA")
}

// TestSubscriptionOnly_ExhaustedSubscription_Refuses402: in subscription-only
// mode, a turn that can't stay on the subscription (here: the sub is exhausted,
// so the bypass disengages) must be refused with the credits-exhausted sentinel
// rather than dispatched to a paid model against the negative balance.
func TestSubscriptionOnly_ExhaustedSubscription_Refuses402(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Weekly window exhausted so the bypass disengages.
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec, req, body := bypassRequest(t)
	ctx := billing.WithSubscriptionOnly(bypassCtx(0.80))
	err := svc.ProxyMessages(ctx, body, rec, req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable),
		"an exhausted subscription in subscription-only mode must be refused, not paid for")
	assert.Empty(t, p.proxyBodies, "no paid dispatch may occur when the subscription can't serve the turn")
}

// TestSubscriptionOnly_NonBypassServedOnSub_Serves: a non-bypass turn (here the
// usage-bypass gate is off) below the overdraft floor must still serve free on
// the caller's own Claude OAuth subscription rather than be refused. Hard-pins,
// force-model, and sticky turns win before usage-bypass in runTurnLoop, so
// gating refusal on the bypass flag would 402 turns that run fine on the sub;
// the guard gates on the resolved subscription credential instead.
func TestSubscriptionOnly_NonBypassServedOnSub_Serves(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl}}
	// Streaming upstream so the routing-marker writer (streaming-only) has a
	// stream to prepend the depleted-credits warning to.
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil)

	rec, req, body := bypassRequest(t)
	// Gate disabled (no InstallationUsageBypassContextKey) => usage-bypass never
	// engages; the caller still presents a Claude OAuth subscription and the
	// scorer routes to Anthropic, so the turn serves on that subscription.
	ctx := billing.WithSubscriptionOnly(
		context.WithValue(context.Background(), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken))
	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))

	assert.Positive(t, fr.routeCalls, "a non-bypass turn runs the scorer")
	require.Len(t, p.proxyBodies, 1, "the turn must serve on the subscription exactly once (no paid failover)")
	require.NotNil(t, p.proxyCreds[0], "the dispatch must carry the caller's subscription credential")
	assert.True(t, p.proxyCreds[0].OAuth,
		"a non-bypass turn must serve on the caller's own Claude subscription so billing debits $0")
	assert.Contains(t, rec.Body.String(), "credits are depleted",
		"a served-on-sub non-bypass turn must surface the depleted-credits warning + top-up CTA")
	assert.Contains(t, rec.Body.String(), "router-credits", "the warning must surface the top-up CTA")
}

// TestSubscriptionOnly_NonBypassPaidRoute_Refuses402: a non-bypass turn that
// routing resolves to a paid provider (no covering subscription credential)
// must be refused with the credits-exhausted sentinel and never dispatched —
// paid failover is disabled below the floor.
func TestSubscriptionOnly_NonBypassPaidRoute_Refuses402(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenRouter, Model: "deepseek/deepseek-chat", Reason: "cluster:v0.2"}}
	p := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","type":"message"}`))
	}}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderOpenRouter: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil)

	rec, req, body := bypassRequest(t)
	// Sub present but the scorer routes to a paid provider it can't cover.
	ctx := billing.WithSubscriptionOnly(
		context.WithValue(context.Background(), proxy.AnthropicSubscriptionContextKey{}, bypassSubToken))
	err := svc.ProxyMessages(ctx, body, rec, req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable),
		"a non-bypass turn that routes to a paid model must be refused, not dispatched")
	assert.Empty(t, p.proxyBodies, "no paid dispatch may occur below the floor in subscription-only mode")
}

// TestSubscriptionOnly_BypassRetryable_Refuses402: when the bypass attempt hits
// a retryable upstream error (e.g. a 429 the moment the weekly limit binds), the
// normal path reroutes to a paid model. In subscription-only mode that reroute
// is forbidden — the turn must be refused instead of billed.
func TestSubscriptionOnly_BypassRetryable_Refuses402(t *testing.T) {
	bypassResp := &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Headers: http.Header{
			"anthropic-ratelimit-unified-weekly-limit":     []string{"100000"},
			"anthropic-ratelimit-unified-weekly-reset":     []string{"2025-12-31T00:00:00Z"},
			"anthropic-ratelimit-unified-weekly-remaining": []string{"0"},
		},
		Body: []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit exceeded"}}`),
	}
	p := &fakeProvider{proxyErr: bypassResp}
	wrappedP := &swapErrProvider{first: bypassResp, second: nil, inner: p}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: bypassScorerPickMdl, Reason: "cluster:v0.2"}}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Under threshold so the bypass engages for the first attempt.
	obs.Record(obs.Key([]byte(bypassSubToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: wrappedP}, nil, false, nil, nil, false, providers.ProviderAnthropic, bypassScorerPickMdl, nil).
		WithSubscriptionAwareRouting(obs, 0.05, 2.0)

	rec, req, body := bypassRequest(t)
	ctx := billing.WithSubscriptionOnly(bypassCtx(0.80))
	err := svc.ProxyMessages(ctx, body, rec, req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrCreditsExhaustedSubscriptionUnavailable),
		"a retryable bypass failure in subscription-only mode must be refused, not rerouted to a paid model")
	assert.Equal(t, 0, fr.routeCalls, "subscription-only mode must not run the scorer to reroute onto a paid model")
	assert.Equal(t, 1, wrappedP.calls, "only the bypass attempt may dispatch; no paid reroute")
}

// swapErrProvider wraps a fakeProvider and returns `first` as the proxyErr on
// the first dispatch, `second` on every dispatch thereafter. Used to simulate a
// bypass 429 followed by a successful routed dispatch against the same fake.
type swapErrProvider struct {
	first, second error
	inner         *fakeProvider
	calls         int
}

func (s *swapErrProvider) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	s.calls++
	err := s.first
	if s.calls > 1 {
		err = s.second
	}
	orig := s.inner.proxyErr
	s.inner.proxyErr = err
	defer func() { s.inner.proxyErr = orig }()
	return s.inner.Proxy(ctx, decision, prep, w, r)
}

func (s *swapErrProvider) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return s.inner.Passthrough(ctx, prep, w, r)
}
