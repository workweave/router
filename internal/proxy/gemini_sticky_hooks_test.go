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

// The force-model header writes a user-forced pin on the Gemini surface.
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

// A non-retryable upstream error on a sticky Gemini pin increments its
// eviction counter.
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
