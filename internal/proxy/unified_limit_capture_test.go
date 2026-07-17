package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
)

func TestUnifiedLimitHeadersFrom_NoCaptureInstalled(t *testing.T) {
	// A context that never went through withUnifiedLimitCapture must report
	// nothing rather than panic — callers (unifiedLimitHeadersJSON) rely on
	// this to leave the telemetry column NULL on any code path that doesn't
	// route through ProxyMessages' capture wiring.
	assert.Nil(t, UnifiedLimitHeadersFrom(context.Background()))
}

func TestCaptureUnifiedLimitHeaders_RecordsOnSubscriptionCredential(t *testing.T) {
	ctx := withUnifiedLimitCapture(context.Background())
	ctx = context.WithValue(ctx, CredentialsContextKey{}, &Credentials{
		APIKey: []byte("sk-ant-oat01-live"), Source: credSourceSubscription, OAuth: true,
	})

	resp := http.Header{}
	resp.Set("anthropic-ratelimit-unified-status", "allowed")
	resp.Set("anthropic-ratelimit-unified-overage-status", "rejected")
	resp.Set("content-type", "application/json") // must NOT leak into the captured set

	captureUnifiedLimitHeaders(ctx, resp)

	got := UnifiedLimitHeadersFrom(ctx)
	require.NotNil(t, got)
	assert.Equal(t, "allowed", got["anthropic-ratelimit-unified-status"])
	assert.Equal(t, "rejected", got["anthropic-ratelimit-unified-overage-status"])
	if _, ok := got["content-type"]; ok {
		t.Errorf("non-unified header leaked into capture: %v", got)
	}
}

func TestCaptureUnifiedLimitHeaders_SkipsNonOAuthCredential(t *testing.T) {
	// A BYOK/deployment-key call (e.g. the handover summarizer after
	// clearCredentials) must not be recorded — those headers describe a
	// different account's quota, not the request's own subscription.
	ctx := withUnifiedLimitCapture(context.Background())
	ctx = context.WithValue(ctx, CredentialsContextKey{}, &Credentials{
		APIKey: []byte("sk-ant-api-deployment"), Source: credSourceBYOK, OAuth: false,
	})

	resp := http.Header{}
	resp.Set("anthropic-ratelimit-unified-status", "allowed")
	captureUnifiedLimitHeaders(ctx, resp)

	assert.Nil(t, UnifiedLimitHeadersFrom(ctx), "non-OAuth credential must not be captured")
}

func TestCaptureUnifiedLimitHeaders_NoCredentialsResolved(t *testing.T) {
	ctx := withUnifiedLimitCapture(context.Background())
	resp := http.Header{}
	resp.Set("anthropic-ratelimit-unified-status", "allowed")
	captureUnifiedLimitHeaders(ctx, resp) // no CredentialsContextKey set at all

	assert.Nil(t, UnifiedLimitHeadersFrom(ctx))
}

func TestCaptureUnifiedLimitHeaders_NoUnifiedHeadersPresent(t *testing.T) {
	// A subscription-served response with no unified headers at all (e.g. an
	// endpoint other than /v1/messages) must leave the capture empty rather
	// than recording an empty-but-non-nil map.
	ctx := withUnifiedLimitCapture(context.Background())
	ctx = context.WithValue(ctx, CredentialsContextKey{}, &Credentials{
		APIKey: []byte("sk-ant-oat01-live"), Source: credSourceSubscription, OAuth: true,
	})
	captureUnifiedLimitHeaders(ctx, http.Header{"Content-Type": {"application/json"}})

	assert.Nil(t, UnifiedLimitHeadersFrom(ctx))
}

func TestUnifiedLimitHeadersJSON(t *testing.T) {
	t.Run("nothing captured -> nil (NULL column)", func(t *testing.T) {
		ctx := withUnifiedLimitCapture(context.Background())
		assert.Nil(t, unifiedLimitHeadersJSON(ctx))
	})

	t.Run("captured set round-trips through JSON", func(t *testing.T) {
		ctx := withUnifiedLimitCapture(context.Background())
		ctx = context.WithValue(ctx, CredentialsContextKey{}, &Credentials{
			APIKey: []byte("sk-ant-oat01-live"), Source: credSourceSubscription, OAuth: true,
		})
		resp := http.Header{}
		resp.Set("anthropic-ratelimit-unified-status", "allowed")
		resp.Set("anthropic-ratelimit-unified-5h-utilization", "0.42")
		captureUnifiedLimitHeaders(ctx, resp)

		b := unifiedLimitHeadersJSON(ctx)
		require.NotNil(t, b)
		var got map[string]string
		require.NoError(t, json.Unmarshal(b, &got))
		assert.Equal(t, "allowed", got["anthropic-ratelimit-unified-status"])
		assert.Equal(t, "0.42", got["anthropic-ratelimit-unified-5h-utilization"])
	})
}

// End-to-end through the shared observer composition: a Claude Code
// subscription request with NO subscription-aware routing configured
// (s.usageObserver == nil) must still capture the raw unified headers. This is
// the scenario Phase 0 dogfooding actually runs under before the subsidy
// feature and this instrumentation are necessarily enabled together.
func TestWithUsageObserver_CapturesRawHeadersEvenWithoutSubsidyObserver(t *testing.T) {
	s := &Service{} // usageObserver is nil: subsidy feature not configured

	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-ant-oat01-claudecode")

	ctx := s.withUsageObserver(context.Background(), headers)
	callCtx := context.WithValue(ctx, CredentialsContextKey{}, &Credentials{
		APIKey: []byte("sk-ant-oat01-claudecode"), Source: credSourceSubscription, OAuth: true,
	})

	resp := http.Header{}
	resp.Set("anthropic-ratelimit-unified-status", "allowed")
	resp.Set("anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled")
	providers.ObserveUpstreamHeaders(callCtx, resp)

	got := UnifiedLimitHeadersFrom(ctx)
	require.NotNil(t, got, "raw headers must be captured independent of the subsidy observer")
	assert.Equal(t, "allowed", got["anthropic-ratelimit-unified-status"])
	assert.Equal(t, "org_level_disabled", got["anthropic-ratelimit-unified-overage-disabled-reason"])
}

// withUsageObserver must remain a true no-op (return ctx unchanged) when there
// is genuinely nothing to do: no subsidy observer configured AND no Anthropic
// subscription token detected on the request. This preserves the pre-existing
// contract other tests (TestSubsidyFactors_DisabledForInstallation etc.) rely
// on, and confirms Phase 0 instrumentation doesn't start capturing on every
// request regardless of auth.
func TestWithUsageObserver_NoopWhenNothingToObserve(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	got := s.withUsageObserver(ctx, http.Header{}) // no Authorization at all
	assert.Nil(t, UnifiedLimitHeadersFrom(got), "no subscription token present -> capture never installed")
}
