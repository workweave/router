package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bypassFakeProvider is an internal-package fake providers.Client for
// bypassToAnthropic tests. It records the response writer it received so a
// test can assert no bytes were committed on a retryable error.
type bypassFakeProvider struct {
	proxyErr     error
	dispatches   int
	capturedW    http.ResponseWriter
	capturedBody []byte
	capturedR    *http.Request
	capturedCtx  context.Context
	capturedDec  router.Decision
}

func (f *bypassFakeProvider) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	f.dispatches++
	f.capturedCtx = ctx
	f.capturedDec = decision
	f.capturedW = w
	f.capturedR = r
	f.capturedBody = append([]byte(nil), prep.Body...)
	return f.proxyErr
}

func (f *bypassFakeProvider) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return nil
}

// newBypassService builds a minimal *Service wired only with the Anthropic
// provider for direct bypassToAnthropic tests. The full ProxyMessages path is
// not exercised here; only bypassToAnthropic is.
func newBypassService(p providers.Client) *Service {
	return &Service{
		providers: map[string]providers.Client{providers.ProviderAnthropic: p},
	}
}

func bypassAnthropicEnvelope(t *testing.T) *translate.RequestEnvelope {
	t.Helper()
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	return env
}

// TestBypass_429_ReturnsErrBypassRetryable_NoBytesWritten: when the Anthropic
// upstream returns a buffered 429 (retryable), bypassToAnthropic must return
// errBypassRetryable so the caller falls through to the routed dispatch path.
// Critically, no response bytes may be written to w — the routed path needs a
// pristine writer to retry.
func TestBypass_429_ReturnsErrBypassRetryable_NoBytesWritten(t *testing.T) {
	upstream := &bypassFakeProvider{proxyErr: &providers.UpstreamErrorResponse{
		Status:  http.StatusTooManyRequests,
		Headers: http.Header{"anthropic-ratelimit-unified-weekly-limit": []string{"100000"}},
		Body:    []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"weekly limit"}}`),
	}}
	svc := newBypassService(upstream)

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)

	assert.ErrorIs(t, err, errBypassRetryable, "a retryable 429 must signal fall-through to routed dispatch")
	assert.Equal(t, 1, upstream.dispatches, "the bypass attempt must hit the upstream exactly once")

	// No status code, no body bytes — the routed path needs a pristine writer.
	assert.Equal(t, http.StatusOK, rec.Code, "httptest.Recorder defaults to 200 — WriteHeader must not have been called")
	assert.Empty(t, rec.Body.Bytes(), "no error body must be flushed on a retryable bypass failure")
}

// TestBypass_NonRetryableError_StillFlushes: a 400 from Anthropic is the
// caller's fault (malformed request); rerouting would mask the bug. The bypass
// path must still flush it as the real upstream status+body, returning nil.
func TestBypass_NonRetryableError_StillFlushes(t *testing.T) {
	upstream := &bypassFakeProvider{proxyErr: &providers.UpstreamErrorResponse{
		Status: http.StatusBadRequest,
		Body:   []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad model"}}`),
	}}
	svc := newBypassService(upstream)

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)

	require.NoError(t, err, "a non-retryable 400 must flush and return nil — rerouting would mask a malformed request")
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the 400 must be flushed to the client verbatim")
	assert.Contains(t, rec.Body.String(), "invalid_request_error")
}

// TestBypass_NilError_ReturnsNil: success path — bypass completes, returns nil.
func TestBypass_NilError_ReturnsNil(t *testing.T) {
	upstream := &bypassFakeProvider{} // no error, no response writer action
	svc := newBypassService(upstream)

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)
	require.NoError(t, err)
}

// TestBypass_TransportError_ReroutesViaScorer: a raw transport error (connection
// reset, TLS timeout, etc.) from the upstream proxy call is classified by
// providers.IsRetryable as retryable, so bypassToAnthropic must return
// errBypassRetryable to let the caller fall through to the routed dispatch path.
// No bytes are written to w, so the routed path gets a pristine writer.
func TestBypass_TransportError_ReroutesViaScorer(t *testing.T) {
	transportErr := errors.New("connection reset")
	upstream := &bypassFakeProvider{proxyErr: transportErr}
	svc := newBypassService(upstream)

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)

	assert.ErrorIs(t, err, errBypassRetryable, "a transport error must signal fall-through to routed dispatch")
	assert.Equal(t, 1, upstream.dispatches, "the bypass attempt must hit the upstream exactly once")
	assert.Equal(t, http.StatusOK, rec.Code, "no status header must be written — the routed path needs a pristine writer")
	assert.Empty(t, rec.Body.Bytes(), "no body bytes must be flushed on a transport-error bypass failure")
}

// TestBypass_LocalPrepError_PropagatesToClient: a local preparation error from
// bypassToAnthropic (provider not configured, emit-body failure) must NOT
// trigger reroute via the scorer. The client must see the real failure. This
// guards against regressions where providers.IsRetryable treats build errors as
// retryable and silently reroutes.
func TestBypass_LocalPrepError_PropagatesToClient(t *testing.T) {
	// Wire a service WITHOUT the Anthropic provider to trigger the
	// provider-not-configured prep error path.
	svc := &Service{providers: map[string]providers.Client{}}

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)

	require.Error(t, err, "provider-not-configured must surface as a real error")
	assert.NotErrorIs(t, err, errBypassRetryable, "local prep errors must not trigger reroute — the client must see them")
}

// subscriptionCtx returns a ctx carrying a Claude subscription token (as the
// auth middleware would stash it from X-Weave-Anthropic-Subscription) plus a
// non-empty installation id so resolveAndInjectCredentials takes the
// router-keyed subscription-first branch.
func subscriptionCtx() context.Context {
	ctx := context.WithValue(context.Background(), AnthropicSubscriptionContextKey{}, "sk-ant-oat01-test-subscription-token")
	return context.WithValue(ctx, InstallationIDContextKey{}, "11111111-1111-1111-1111-111111111111")
}

// TestSubscriptionFailover_EligibilityAndSuppression covers the three
// load-bearing predicates of the subscription-credit failover added for the
// 429/header-timeout bug: a subscription-served Anthropic turn is detected as
// such, a deployment Anthropic key counts as a fallback, and suppressing the
// subscription flips credential resolution onto the deployment key.
func TestSubscriptionFailover_EligibilityAndSuppression(t *testing.T) {
	// A request whose Anthropic credential resolves to the caller's subscription.
	ctx := resolveAndInjectCredentials(subscriptionCtx(), providers.ProviderAnthropic, http.Header{})
	require.True(t, servedOnSubscription(ctx), "a resolved subscription token must report servedOnSubscription")

	t.Run("no fallback key: not eligible", func(t *testing.T) {
		s := &Service{} // no deployment Anthropic key, no BYOK
		assert.False(t, s.anthropicFallbackKeyAvailable(ctx),
			"without a Weave/BYOK Anthropic key there is nothing to fail over to")
	})

	t.Run("deployment Anthropic key present: eligible", func(t *testing.T) {
		s := &Service{deploymentKeyedProviders: map[string]struct{}{providers.ProviderAnthropic: {}}}
		assert.True(t, s.anthropicFallbackKeyAvailable(ctx),
			"a deployment Anthropic key is a valid failover target for a throttled subscription")
	})

	t.Run("suppression flips resolution off the subscription", func(t *testing.T) {
		// After withSuppressedClaudeSubscription, resolution must NOT pick the
		// subscription token — so the retry dispatches on the deployment key and
		// servedOnSubscription reports false (billed at full cost, not sub rate).
		suppressed := withSuppressedClaudeSubscription(subscriptionCtx())
		suppressed = resolveAndInjectCredentials(suppressed, providers.ProviderAnthropic, http.Header{})
		assert.False(t, servedOnSubscription(suppressed),
			"a suppressed subscription must not resolve back as the served credential")
	})
}
