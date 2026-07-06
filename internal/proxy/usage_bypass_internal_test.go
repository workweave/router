package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	tracev1 "go.opentelemetry.io/proto/otlp/trace/v1"
)

// bypassFakeProvider is an internal-package fake providers.Client for
// bypassToAnthropic tests. It records the response writer it received so a
// test can assert no bytes were committed on a retryable error.
type bypassFakeProvider struct {
	proxyErr     error
	respBody     string
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
	if f.respBody != "" {
		_, _ = io.WriteString(w, f.respBody)
	}
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

// bypassSpanCollector is an in-process OTLP endpoint that records spans by name for assertion.
type bypassSpanCollector struct {
	srv    *httptest.Server
	mu     sync.Mutex
	byName map[string][]*tracev1.Span
}

func newBypassSpanCollector(t *testing.T) *bypassSpanCollector {
	t.Helper()
	c := &bypassSpanCollector{byName: make(map[string][]*tracev1.Span)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req coltracepb.ExportTraceServiceRequest
		require.NoError(t, proto.Unmarshal(body, &req))
		c.mu.Lock()
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, sp := range ss.Spans {
					c.byName[sp.Name] = append(c.byName[sp.Name], sp)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func spanStr(t *testing.T, sp *tracev1.Span, key string) string {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			sv, ok := kv.Value.Value.(*commonv1.AnyValue_StringValue)
			require.True(t, ok, "attr %q must be a string", key)
			return sv.StringValue
		}
	}
	t.Fatalf("attr %q not present on span", key)
	return ""
}

func spanInt(t *testing.T, sp *tracev1.Span, key string) int64 {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			iv, ok := kv.Value.Value.(*commonv1.AnyValue_IntValue)
			require.True(t, ok, "attr %q must be an int", key)
			return iv.IntValue
		}
	}
	t.Fatalf("attr %q not present on span", key)
	return 0
}

func spanFloat(t *testing.T, sp *tracev1.Span, key string) float64 {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			dv, ok := kv.Value.Value.(*commonv1.AnyValue_DoubleValue)
			require.True(t, ok, "attr %q must be a double", key)
			return dv.DoubleValue
		}
	}
	t.Fatalf("attr %q not present on span", key)
	return 0
}

func spanBool(t *testing.T, sp *tracev1.Span, key string) bool {
	t.Helper()
	for _, kv := range sp.Attributes {
		if kv.Key == key {
			bv, ok := kv.Value.Value.(*commonv1.AnyValue_BoolValue)
			require.True(t, ok, "attr %q must be a bool", key)
			return bv.BoolValue
		}
	}
	t.Fatalf("attr %q not present on span", key)
	return false
}

// TestBypass_EmitsUsageAndCost guards that the bypass span carries token usage
// + catalog-priced cost — subscription turns are invisible to the savings metric without it.
func TestBypass_EmitsUsageAndCost(t *testing.T) {
	const (
		inputTokens  = 1200
		outputTokens = 340
		model        = "claude-sonnet-4-6"
	)
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":1200,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":340}}` + "\n\n"

	collector := newBypassSpanCollector(t)
	emitter, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      collector.srv.URL,
		Workers:       1,
		QueueSize:     100,
		BatchSize:     1,
		FlushInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = emitter.Shutdown(context.Background()) })

	upstream := &bypassFakeProvider{respBody: sse}
	svc := newBypassService(upstream)
	svc.emitter = emitter // makes usageRequired() true so the extractor runs

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	buf := otel.NewBuffer(emitter)
	ctx := buf.WithContext(context.Background())
	err = svc.bypassToAnthropic(ctx, env, feats, false, time.Now(), "req-1", "ext-1", req, rec)
	require.NoError(t, err)
	buf.Flush()

	require.Eventually(t, func() bool {
		collector.mu.Lock()
		defer collector.mu.Unlock()
		return len(collector.byName["router.usage_bypass"]) == 1
	}, time.Second, 5*time.Millisecond, "the bypass span must be exported")

	collector.mu.Lock()
	sp := collector.byName["router.usage_bypass"][0]
	collector.mu.Unlock()

	assert.Equal(t, model, spanStr(t, sp, "requested.model"), "bypass requested model IS the served model")
	assert.Equal(t, model, spanStr(t, sp, "decision.model"))
	assert.Equal(t, int64(inputTokens), spanInt(t, sp, "usage.input_tokens"))
	assert.Equal(t, int64(outputTokens), spanInt(t, sp, "usage.output_tokens"))
	// No subscription credential here, but spanBool fatals if the attribute is absent — guards the emitted contract.
	assert.False(t, spanBool(t, sp, "cost.subscription_served"))

	pricing, ok := catalog.PriceFor(providers.ProviderAnthropic, model)
	require.True(t, ok, "test model must have catalog pricing")
	wantOut := catalog.EffectiveOutputCost(outputTokens, pricing.OutputUSDPer1M)
	wantIn := catalog.EffectiveInputCost(inputTokens, 0, 0, pricing.InputUSDPer1M, pricing, providers.ProviderAnthropic)

	// Bypass never substitutes the model, so requested == actual on the span;
	// Weave zeroes actual downstream when subscription_served is set.
	assert.InDelta(t, wantOut, spanFloat(t, sp, "cost.actual_output_usd"), 1e-9)
	assert.InDelta(t, wantIn, spanFloat(t, sp, "cost.actual_input_usd"), 1e-9)
	assert.InDelta(t, wantOut, spanFloat(t, sp, "cost.requested_output_usd"), 1e-9)
	assert.InDelta(t, wantIn, spanFloat(t, sp, "cost.requested_input_usd"), 1e-9)
}
