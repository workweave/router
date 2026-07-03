package anthropic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	anthropicapi "workweave/router/internal/api/anthropic"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRouter lets tests control exactly what routing decision (or error) the
// proxy.Service's scorer step returns, without needing a real cluster router.
type fakeRouter struct {
	decision router.Decision
	err      error
}

func (f *fakeRouter) Route(_ context.Context, _ router.Request) (router.Decision, error) {
	return f.decision, f.err
}

// fakeProviderClient is a minimal providers.Client double: Proxy/Passthrough
// return whatever the test wants, optionally writing a response first.
type fakeProviderClient struct {
	proxyErr       error
	proxyStatus    int
	proxyBody      string
	passthroughErr error
}

func (f *fakeProviderClient) Proxy(_ context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if f.proxyErr != nil {
		return f.proxyErr
	}
	status := f.proxyStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(f.proxyBody))
	return nil
}

func (f *fakeProviderClient) Passthrough(_ context.Context, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if f.passthroughErr != nil {
		return f.passthroughErr
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(f.proxyBody))
	return nil
}

// max_tokens > 256 so DetectFromEnvelope treats this as a MainLoop turn, not a probe/classifier.
const validAnthropicBody = `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`

// newTestService wires a proxy.Service with a fake router and optional fake provider, no real I/O.
func newTestService(r router.Router, clientName string, client providers.Client) *proxy.Service {
	providerMap := map[string]providers.Client{}
	if clientName != "" && client != nil {
		providerMap[clientName] = client
	}
	return proxy.NewService(r, providerMap, nil, false, nil, nil, false, "", "", nil)
}

func errorEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "error", got["type"])
	errObj, ok := got["error"].(map[string]any)
	require.True(t, ok, "expected error envelope to carry an \"error\" object")
	return errObj
}

func messagesEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages", anthropicapi.MessagesHandler(svc, nil))
	return engine
}

func postMessages(engine *gin.Engine, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)))
	return rec
}

func TestMessagesHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := messagesEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := postMessages(engine, oversized)

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_MalformedBodyReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := messagesEngine(svc)

	// A JSON array is valid JSON but not a JSON object, so ParseAnthropic
	// returns translate.ErrNotJSONObject.
	rec := postMessages(engine, []byte(`[]`))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_NoEligibleProviderReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrNoEligibleProvider}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_InvalidRoutingKnobsReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrInvalidRoutingKnobs}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestMessagesHandler_ClusterUnavailableReturns503WithRetryAfter(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrClusterUnavailable}, "", nil)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_RLPolicyUnavailableReturns503(t *testing.T) {
	// No RL router wired — strategy fails closed rather than falling back to cluster scorer.
	svc := newTestService(&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5"}}, "", nil)
	engine := messagesEngine(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(validAnthropicBody)))
	req = req.WithContext(router.WithStrategy(req.Context(), router.StrategyRL))
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
	assert.Contains(t, errObj["message"], "RL policy router")
}

func TestMessagesHandler_BanditUnavailableReturns503(t *testing.T) {
	// No bandit router wired, mirroring the RL case above.
	svc := newTestService(&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5"}}, "", nil)
	engine := messagesEngine(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(validAnthropicBody)))
	req = req.WithContext(router.WithStrategy(req.Context(), router.StrategyBandit))
	engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
	assert.Contains(t, errObj["message"], "bandit router")
}

func TestMessagesHandler_UpstreamStatusErrorPassesThroughStatus(t *testing.T) {
	client := &fakeProviderClient{proxyErr: &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_UnknownErrorReturns502(t *testing.T) {
	client := &fakeProviderClient{proxyErr: errors.New("boom: transport exploded")}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestMessagesHandler_HappyPathServesUpstreamResponse(t *testing.T) {
	client := &fakeProviderClient{proxyStatus: http.StatusOK, proxyBody: `{"id":"msg_1","type":"message"}`}
	svc := newTestService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-5", Reason: "test_reason"}},
		providers.ProviderAnthropic, client,
	)
	engine := messagesEngine(svc)

	rec := postMessages(engine, []byte(validAnthropicBody))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"id":"msg_1","type":"message"}`, rec.Body.String())
	assert.Equal(t, "claude-sonnet-4-5", rec.Header().Get("x-router-model"))
	assert.Equal(t, providers.ProviderAnthropic, rec.Header().Get("x-router-provider"))
	assert.Equal(t, "test_reason", rec.Header().Get("x-router-decision"))
}

// --- RouteHandler (/v1/route) ---

func routeEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/route", anthropicapi.RouteHandler(svc))
	return engine
}

func TestRouteHandler_InvalidRoutingKnobsReturns400(t *testing.T) {
	svc := newTestService(&fakeRouter{err: cluster.ErrInvalidRoutingKnobs}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusBadRequest, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_request_error", errObj["type"])
}

func TestRouteHandler_GenericRoutingErrorReturns502(t *testing.T) {
	svc := newTestService(&fakeRouter{err: errors.New("scorer exploded")}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusBadGateway, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestRouteHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := routeEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader(oversized)))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestRouteHandler_HappyPathReturnsDecision(t *testing.T) {
	decision := router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cheap_and_cheerful"}
	svc := newTestService(&fakeRouter{decision: decision}, "", nil)
	engine := routeEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/route", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "claude-haiku-4-5", got["model"])
	assert.Equal(t, providers.ProviderAnthropic, got["provider"])
	assert.Equal(t, "cheap_and_cheerful", got["reason"])
}

// --- PassthroughHandler ---

func passthroughEngine(svc *proxy.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/messages/count_tokens", anthropicapi.PassthroughHandler(svc))
	return engine
}

func TestPassthroughHandler_NotImplementedReturns501(t *testing.T) {
	client := &fakeProviderClient{passthroughErr: providers.ErrNotImplemented}
	svc := newTestService(&fakeRouter{}, providers.ProviderAnthropic, client)
	engine := passthroughEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusNotImplemented, rec.Code)
	errObj := errorEnvelope(t, rec.Body.Bytes())
	assert.Equal(t, "api_error", errObj["type"])
}

func TestPassthroughHandler_RequestTooLarge(t *testing.T) {
	svc := newTestService(&fakeRouter{}, "", nil)
	engine := passthroughEngine(svc)

	oversized := bytes.Repeat([]byte("a"), 10*1024*1024+1)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(oversized)))

	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestPassthroughHandler_HappyPath(t *testing.T) {
	client := &fakeProviderClient{proxyBody: `{"input_tokens":42}`}
	svc := newTestService(&fakeRouter{}, providers.ProviderAnthropic, client)
	engine := passthroughEngine(svc)

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader([]byte(validAnthropicBody))))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `{"input_tokens":42}`, rec.Body.String())
}
