package proxy_test

import (
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

	assert.Equal(t, "gemini-2.5-pro", rec.Header().Get("x-router-model"))
	assert.Equal(t, providers.ProviderGoogle, rec.Header().Get("x-router-provider"))
	require.Len(t, googleProv.proxyBodies, 1, "the upstream Google client must be invoked once")
	body := string(googleProv.proxyBodies[0])
	assert.NotContains(t, body, `"model"`,
		"model is encoded in the upstream URL, not the body")
	assert.NotContains(t, body, `"stream"`,
		"streaming is signalled via GeminiStreamHintHeader")
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
