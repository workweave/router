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

// TestBypass_TransportError_NotFlushed: a non-UpstreamErrorResponse transport
// error is not an *UpstreamErrorResponse, so the errors.As branches both miss
// and the raw error is returned. The caller will see a non-errBypassRetryable
// error and propagate it (matching pre-change behavior for unexpected errors).
func TestBypass_TransportError_NotTreatedAsRetryable(t *testing.T) {
	transportErr := errors.New("connection reset")
	upstream := &bypassFakeProvider{proxyErr: transportErr}
	svc := newBypassService(upstream)

	env := bypassAnthropicEnvelope(t)
	feats := env.RoutingFeatures(false)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	err := svc.bypassToAnthropic(context.Background(), env, feats, false, time.Now(), "req-1", "ext-1", req, rec)

	require.ErrorIs(t, err, transportErr, "a raw transport error must propagate unchanged")
	assert.NotErrorIs(t, err, errBypassRetryable, "transport errors are not the bypass-retryable sentinel")
}
