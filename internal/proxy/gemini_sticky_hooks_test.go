package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// geminiPinTestBody is a minimal, valid Gemini-native request body used by
// both tests below.
const geminiPinTestBody = `{
	"contents":[{"role":"user","parts":[{"text":"hello"}]}]
}`

func newGeminiPinSvc(fr *fakeRouter, store *fakePinStore) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: &fakeProvider{}},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderGoogle,
		"gemini-2.5-flash",
		nil,
	)
}

// TestProxyGeminiGenerateContent_ForceModelHeader_WritesUserForcedPin proves
// bug #730 finding 1: x-weave-force-model is a documented escape hatch for
// headless clients (see ForceModelHeader doc comment in force_model.go) and
// is honored on /v1/messages (see
// TestService_ForceModelHeader_WritesUserForcedPin in
// service_session_pin_test.go), but ProxyGeminiGenerateContent never calls
// applyForceModelHeader, so the header is silently ignored on the Gemini
// surface. This test currently FAILS: no store.upserts entry has
// Reason == translate.ReasonUserForceModel, because the call site is
// missing in gemini.go.
func TestProxyGeminiGenerateContent_ForceModelHeader_WritesUserForcedPin(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "fresh"}}
	svc := newGeminiPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(""))
	httpReq.Header.Set(proxy.ForceModelHeader, "gemini-2.5-flash")
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, []byte(geminiPinTestBody), rec, httpReq))

	store.mu.Lock()
	defer store.mu.Unlock()
	var forced *sessionpin.Pin
	for i := range store.upserts {
		if store.upserts[i].Reason == translate.ReasonUserForceModel {
			forced = &store.upserts[i]
			break
		}
	}
	require.NotNil(t, forced, "x-weave-force-model header must write a user_forced pin upsert on the Gemini surface, same as it does on /v1/messages and /v1/chat/completions")
	assert.Equal(t, "gemini-2.5-flash", forced.Model)
	assert.Equal(t, providers.ProviderGoogle, forced.Provider)
}

// TestProxyGeminiGenerateContent_MaybeEvictPinAfterUpstreamErr_Called proves
// bug #730 finding 2: maybeEvictPinAfterUpstreamErr (the two-strike pin
// eviction policy) is called by both ProxyMessages and
// ProxyOpenAIChatCompletion after every dispatch, but never by
// ProxyGeminiGenerateContent. We can't observe the increment/reset counters
// directly (unexported), but we CAN observe that fakePinStore.IncrementUpstreamErrors
// is never invoked by asserting on the exported-via-test-helper call count.
// This test currently FAILS to show any eviction-path activity: incrementCalls
// stays at 0 regardless of the upstream error, because the call site is
// missing in gemini.go.
func TestProxyGeminiGenerateContent_MaybeEvictPinAfterUpstreamErr_Called(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", PinnedUntil: time.Now().Add(time.Hour)}
	failingProv := &fakeProvider{proxyErr: &providers.UpstreamErrorResponse{Status: http.StatusBadRequest}}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-2.5-pro", Reason: "sticky"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: failingProv},
		nil, false, nil,
		store,
		false, providers.ProviderGoogle, "gemini-2.5-flash",
		nil,
	)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(""))
	_ = svc.ProxyGeminiGenerateContent(ctx, []byte(geminiPinTestBody), rec, httpReq)

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Greater(t, store.incrementCalls, 0,
		"a non-retryable upstream 4xx on a sticky Gemini pin must call maybeEvictPinAfterUpstreamErr (IncrementUpstreamErrors), same as it does on /v1/messages and /v1/chat/completions")
}
