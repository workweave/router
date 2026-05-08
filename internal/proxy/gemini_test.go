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

// The handler injects "model" and "stream" before this method runs, so
// the test passes the post-injection body shape.
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
		nil, false, 0, nil, nil,
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
	// Synthetic injected fields must not reach the upstream body.
	body := string(googleProv.proxyBodies[0])
	assert.NotContains(t, body, `"model"`,
		"the model field is encoded in the upstream URL, not the body")
	assert.NotContains(t, body, `"stream"`,
		"streaming choice is signalled via the GeminiStreamHintHeader")
}

func TestProxyGeminiGenerateContent_CrossFormatReturnsSentinel(t *testing.T) {
	store := newFakePinStore()
	// Decision picks Anthropic — cross-format emit from a Gemini envelope
	// is intentionally deferred. The handler maps the sentinel to HTTP 501.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderGoogle:    &fakeProvider{},
		},
		nil, false, 0, nil, nil,
		store,
		false, providers.ProviderGoogle, "gemini-2.5-flash",
		nil,
	)

	ctx := authedCtx("00000000-0000-0000-0000-000000000001")
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-pro:generateContent", strings.NewReader(""))
	err := svc.ProxyGeminiGenerateContent(ctx, []byte(geminiInjectedBody), rec, httpReq)

	require.Error(t, err)
	assert.True(t, errors.Is(err, proxy.ErrGeminiCrossFormatUnsupported),
		"cross-format Gemini-in→non-Google routing must surface the typed sentinel "+
			"so the handler can map it to HTTP 501")
}
