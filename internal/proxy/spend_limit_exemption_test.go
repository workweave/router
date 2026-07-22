package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capturingBillingRepo reports the engineer's monthly cap as reached and records
// every DebitInference so a test can assert the served turn debited $0. Only the
// methods checkUserMonthlySpendLimit and the debit hook touch matter; the rest
// satisfy the billing.Repo interface.
type capturingBillingRepo struct {
	userSpent int64
	userLimit *int64

	mu     sync.Mutex
	debits []billing.DebitParams
}

func (r *capturingBillingRepo) GetBalance(context.Context, string) (int64, error) { return 0, nil }
func (r *capturingBillingRepo) HasActiveOverride(context.Context, string) (bool, error) {
	return false, nil
}
func (r *capturingBillingRepo) DebitInference(_ context.Context, p billing.DebitParams) (int64, error) {
	r.mu.Lock()
	r.debits = append(r.debits, p)
	r.mu.Unlock()
	return 0, nil
}
func (r *capturingBillingRepo) GetAPIKeySpend(context.Context, string) (int64, *int64, bool, error) {
	return 0, nil, false, nil
}
func (r *capturingBillingRepo) GetUserMonthlySpendAndLimit(context.Context, string, string) (int64, *int64, error) {
	return r.userSpent, r.userLimit, nil
}
func (r *capturingBillingRepo) GetOrgMonthlySpendAndLimit(context.Context, string) (int64, *int64, error) {
	return 0, nil, nil
}
func (r *capturingBillingRepo) GetAutopayConfig(context.Context, string) (bool, int64, error) {
	return false, 0, nil
}
func (r *capturingBillingRepo) BillingTablesExist(context.Context) (bool, error) { return true, nil }

func (r *capturingBillingRepo) recordedDebits() []billing.DebitParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]billing.DebitParams(nil), r.debits...)
}

// TestProxyMessages_OverEngineerCap_CoveringSubscription_ServesFreeNo402 is the
// end-to-end contract for the fix: an engineer over the monthly spend cap whose
// org has usage-bypass enabled and who presents a Claude subscription covering
// /v1/messages must NOT be 402'd. The turn serves on the caller's own
// subscription and the debit is $0.
func TestProxyMessages_OverEngineerCap_CoveringSubscription_ServesFreeNo402(t *testing.T) {
	const subToken = "sk-ant-oat01-covering-subscription"
	limit := int64(10_000_000)
	repo := &capturingBillingRepo{userSpent: 10_002_854, userLimit: &limit} // over the cap

	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6"}}
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	obs.Record(obs.Key([]byte(subToken)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 0.20, WindowMinutes: 300},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageObserver(obs).
		WithBillingService(billing.NewService(repo))

	// The auth middleware resolves these onto ctx: org id, user id, the
	// usage-bypass config, and the Claude subscription token.
	ctx := context.WithValue(context.Background(), proxy.ExternalIDContextKey{}, "org-capped")
	ctx = context.WithValue(ctx, auth.UserIDContextKey{}, "engineer-1")
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{Enabled: true})
	ctx = context.WithValue(ctx, proxy.AnthropicSubscriptionContextKey{}, subToken)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+subToken)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req),
		"an over-cap engineer with a covering subscription must not be 402'd")

	require.Len(t, p.proxyBodies, 1, "the turn must serve on the subscription exactly once")
	require.NotNil(t, p.proxyCreds[0], "the dispatch must carry the caller's subscription credential")
	assert.True(t, p.proxyCreds[0].OAuth, "the turn must serve on the caller's own Claude subscription")
	assert.Contains(t, rec.Body.String(), "credits are depleted",
		"a subscription-only turn must surface the depleted-credits warning")

	for _, d := range repo.recordedDebits() {
		assert.Equal(t, int64(0), d.DeltaUsdMicros,
			"a subscription-served turn must debit $0 even over the engineer cap")
	}
}

// TestProxyMessages_OverEngineerCap_NoSubscription_402s is the counterpart: the
// same over-cap engineer WITHOUT a subscription credential must still be
// rejected — the turn would route to a paid model, which the cap must bound.
func TestProxyMessages_OverEngineerCap_NoSubscription_402s(t *testing.T) {
	limit := int64(10_000_000)
	repo := &capturingBillingRepo{userSpent: 10_002_854, userLimit: &limit}

	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6"}}
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithBillingService(billing.NewService(repo))

	// Usage-bypass enabled but no subscription credential on the request.
	ctx := context.WithValue(context.Background(), proxy.ExternalIDContextKey{}, "org-capped")
	ctx = context.WithValue(ctx, auth.UserIDContextKey{}, "engineer-1")
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{Enabled: true})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(ctx, body, rec, req)
	require.Error(t, err, "an over-cap engineer with no covering subscription must be rejected")
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
	assert.Empty(t, p.proxyBodies, "no dispatch may occur when the cap rejects the turn")
}

// TestProxyMessages_OverEngineerCap_ExhaustedSubscription_Refuses402 closes the
// loop that the first two tests in this file leave open: an engineer past the
// monthly cap AND the caller's own subscription at its limit must be refused —
// never dispatched to a paid model, never debited. The cap pushes the request
// into subscription-only mode; the subscription-only mode's pre-dispatch guard
// sees the observed-exhausted sub and returns the credits-exhausted sentinel.
// Without this test, the cap→subscription-only handoff and the
// exhausted-sub→refusal handoff are covered separately; this one pins that
// they compose correctly end-to-end so a refactor of either flag site can't
// silently break the corner.
func TestProxyMessages_OverEngineerCap_ExhaustedSubscription_Refuses402(t *testing.T) {
	const subToken = "sk-ant-oat01-covering-subscription"
	limit := int64(10_000_000)
	repo := &capturingBillingRepo{userSpent: 10_002_854, userLimit: &limit} // over the engineer cap

	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6"}}
	p := &fakeProvider{proxyResponse: bypassStreamResponse}
	obs := usage.NewObserver([]byte("salt"), 10*time.Minute, time.Now)
	// Weekly window exhausted: the pre-dispatch guard reads this and refuses
	// before any upstream call, so no doomed round-trip against the would-be
	// spent subscription.
	obs.Record(obs.Key([]byte(subToken)), usage.Snapshot{
		Secondary: usage.Window{UsedPercent: 1.0, WindowMinutes: 10080},
	})
	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageObserver(obs).
		WithBillingService(billing.NewService(repo))

	ctx := context.WithValue(context.Background(), proxy.ExternalIDContextKey{}, "org-capped")
	ctx = context.WithValue(ctx, auth.UserIDContextKey{}, "engineer-1")
	ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{Enabled: true})
	ctx = context.WithValue(ctx, proxy.AnthropicSubscriptionContextKey{}, subToken)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+subToken)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(ctx, body, rec, req)
	require.Error(t, err, "over-cap engineer with an exhausted subscription must be refused")
	assert.ErrorIs(t, err, proxy.ErrCreditsExhaustedSubscriptionUnavailable,
		"the refusal must be the credits-exhausted sentinel so the API mapper emits HTTP 402 with the top-up CTA")
	assert.Empty(t, p.proxyBodies, "no dispatch may occur when both the cap and the subscription are spent")
	assert.Empty(t, repo.recordedDebits(),
		"nothing must be debited when the turn is refused before dispatch — paid spend stays bounded at the cap")
}
