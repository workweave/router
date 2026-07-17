package proxy_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const geminiPassthroughBody = `{
	"contents":[{"role":"user","parts":[{"text":"hello"}]}]
}`

// Post-injection body shape: handler adds "model" and "stream" before calling.
const geminiInjectedBody = `{
	"model":"gemini-1.5-pro",
	"stream":false,
	"contents":[{"role":"user","parts":[{"text":"hello"}]}]
}`

func TestProxyGeminiGenerateContent_RoutesToGoogleProvider(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleProv},
		nil, false, nil,
		store,
		false, providers.ProviderGoogle, "gemini-2.5-flash",
		nil,
	)

	ctx := authedCtx("00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, []byte(geminiInjectedBody), rec, httpReq))

	assert.Equal(t, "gemini-2.5-pro", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, providers.ProviderGoogle, rec.Header().Get(proxy.HeaderRouterProvider))
	require.Len(t, googleProv.proxyBodies, 1, "the upstream Google client must be invoked once")
	body := string(googleProv.proxyBodies[0])
	assert.NotContains(t, body, `"model"`,
		"model is encoded in the upstream URL, not the body")
	assert.NotContains(t, body, `"stream"`,
		"streaming is signalled via GeminiStreamHintHeader")
}

func TestProxyGeminiGenerateContent_RestrictsRoutingToGeminiFamily(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderGoogle:    googleProv,
		},
		nil, false, nil,
		store,
		false, providers.ProviderGoogle, "gemini-2.5-flash",
		nil,
	)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(authedCtx("00000000-0000-0000-0000-000000000001"), []byte(geminiInjectedBody), rec, req))

	require.NotNil(t, fr.capturedReq)
	assert.Equal(t, map[string]struct{}{providers.ProviderGoogle: {}}, fr.capturedReq.EnabledProviders)
}

func TestProxyGeminiGenerateContent_CrossFormatReturnsSentinel(t *testing.T) {
	store := newFakePinStore()
	// Cross-format from a Gemini envelope is deferred; handler maps to HTTP 501.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderGoogle:    &fakeProvider{},
		},
		nil, false, nil,
		store,
		false, providers.ProviderGoogle, "gemini-2.5-flash",
		nil,
	)

	ctx := authedCtx("00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", strings.NewReader(""))
	err := svc.ProxyGeminiGenerateContent(ctx, []byte(geminiInjectedBody), rec, httpReq)

	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrGeminiCrossFormatUnsupported))
}

func TestProxyGeminiGenerateContent_DelaysMarkerUntilFirstUpstreamEvent(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"upstream\"}]}}]}\n\n"))
	}}
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}},
		map[string]providers.Client{providers.ProviderGoogle: googleProv},
		nil, false, nil, store, false, providers.ProviderGoogle, "gemini-2.5-flash", nil,
	)
	rec := httptest.NewRecorder()
	body := strings.Replace(geminiInjectedBody, `"stream":false`, `"stream":true`, 1)
	require.NoError(t, svc.ProxyGeminiGenerateContent(authedCtx("00000000-0000-0000-0000-000000000001"), []byte(body), rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:streamGenerateContent", nil)))

	markerAt := strings.Index(rec.Body.String(), "Weave Router")
	upstreamAt := strings.Index(rec.Body.String(), "upstream")
	assert.GreaterOrEqual(t, markerAt, 0)
	assert.GreaterOrEqual(t, upstreamAt, 0)
	assert.Less(t, markerAt, upstreamAt, "the first committed upstream event releases the marker")
}

func TestProxyGeminiGenerateContent_RetriesBuffered429WithoutMarkerLeak(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{proxyErr: &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"message":"retry later"}}`),
	}}
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}},
		map[string]providers.Client{providers.ProviderGoogle: googleProv},
		nil, false, nil, store, false, providers.ProviderGoogle, "gemini-2.5-flash", nil,
	)
	rec := httptest.NewRecorder()
	body := strings.Replace(geminiInjectedBody, `"stream":false`, `"stream":true`, 1)
	err := svc.ProxyGeminiGenerateContent(authedCtx("00000000-0000-0000-0000-000000000001"), []byte(body), rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:streamGenerateContent", nil))
	require.Error(t, err)
	assert.Len(t, googleProv.proxyBodies, 3, "single-provider 429 retries are bounded")
	assert.NotContains(t, rec.Body.String(), "Weave Router", "a retryable upstream failure must not commit the marker")
	assert.Contains(t, rec.Body.String(), "retry later")
}

func TestProxyGeminiGenerateContent_PersistsNonAuthoritativeUsageForReconciliation(t *testing.T) {
	telemetry := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}},
		map[string]providers.Client{providers.ProviderGoogle: &fakeProvider{}},
		nil, false, nil, newFakePinStore(), false, providers.ProviderGoogle, "gemini-2.5-flash", telemetry,
	)
	ctx := context.WithValue(authedCtx("00000000-0000-0000-0000-000000000001"), proxy.ExternalIDContextKey{}, "org-1")

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, []byte(geminiInjectedBody), rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", nil)))

	row := telemetry.firstRow(t)
	assert.Equal(t, "missing", row.UsageAuthorityStatus)
	assert.JSONEq(t, `{"authority_status":"missing"}`, string(row.UsageDetails))
}

func TestProxyGeminiGenerateContent_PersistsToolResultBytes(t *testing.T) {
	telemetry := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "cluster"}},
		map[string]providers.Client{providers.ProviderGoogle: &fakeProvider{}},
		nil, false, nil, newFakePinStore(), false, providers.ProviderGoogle, "gemini-2.5-flash", telemetry,
	)
	ctx := context.WithValue(authedCtx("00000000-0000-0000-0000-000000000001"), proxy.ExternalIDContextKey{}, "org-1")
	body := `{"model":"gemini-1.5-pro","stream":false,"contents":[{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"out":"x"}}}]}]}`

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, []byte(body), rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", nil)))

	row := telemetry.firstRow(t)
	require.NotNil(t, row.ToolResultBytes)
	assert.Equal(t, int32(38), *row.ToolResultBytes)
}
