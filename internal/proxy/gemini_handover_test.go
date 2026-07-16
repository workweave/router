package proxy_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// Gemini SWITCH with a working summarizer must RewriteEnvelope so the
// forwarded body is summary-bounded (not full history).
func TestGeminiSwitch_SummarizerBoundsHistory(t *testing.T) {
	t.Parallel()

	chunk := strings.Repeat("aaaa ", 8000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"text":"ack"}]},
    {"role":"user","parts":[{"text":"now continue with step 2"}]}
  ]
}`)

	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderGoogle,
		Model:           "gemini-3.1-pro-preview",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 10000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderGoogle,
		Model:    "gemini-3.1-flash-lite-preview",
		Reason:   "cluster:v0.2",
	}}
	sz := &fakeSummarizer{summary: "Prior Gemini chat summarized."}
	googleUp := &fakeProvider{}

	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleUp},
		nil, false, nil, store, false,
		providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
	).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.1-pro-preview:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	assert.Equal(t, "gemini-3.1-flash-lite-preview", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, int32(1), sz.calls.Load(), "summarizer must be invoked on Gemini SWITCH")
	require.NotEmpty(t, googleUp.proxyBodies)

	contents := gjson.GetBytes(googleUp.proxyBodies[0], "contents").Array()
	require.Len(t, contents, 2, "expect [summary, latestUser]")
	assert.Equal(t, "model", contents[0].Get("role").String())
	assert.True(t, strings.HasPrefix(contents[0].Get("parts.0.text").String(), translate.HandoverSummaryTag))
	assert.Equal(t, "user", contents[1].Get("role").String())
	assert.Equal(t, "now continue with step 2", contents[1].Get("parts.0.text").String())
	assert.NotContains(t, string(googleUp.proxyBodies[0]), chunk[:40], "long prior user text must be elided")
}

// Mid-tool Gemini SWITCH must not forward an orphan functionResponse after rewrite.
func TestGeminiSwitch_MidToolStripsOrphanFunctionResponse(t *testing.T) {
	t.Parallel()

	chunk := strings.Repeat("aaaa ", 8000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"functionCall":{"name":"edit","args":{"path":"a.go"}}}]},
    {"role":"user","parts":[{"functionResponse":{"name":"edit","response":{"result":"ok"}}}]}
  ]
}`)

	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:        providers.ProviderGoogle,
		Model:           "gemini-3.1-pro-preview",
		Reason:          "cluster:v0.2",
		PinnedUntil:     time.Now().Add(time.Hour),
		LastInputTokens: 10000,
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderGoogle,
		Model:    "gemini-3.1-flash-lite-preview",
		Reason:   "cluster:v0.2",
	}}
	sz := &fakeSummarizer{summary: "User asked to edit a.go; tool ran."}
	googleUp := &fakeProvider{}

	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleUp},
		nil, false, nil, store, false,
		providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
	).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.1-pro-preview:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	require.NotEmpty(t, googleUp.proxyBodies)
	contents := gjson.GetBytes(googleUp.proxyBodies[0], "contents")
	frCount := 0
	contents.ForEach(func(_, entry gjson.Result) bool {
		entry.Get("parts").ForEach(func(_, part gjson.Result) bool {
			if part.Get("functionResponse").Exists() {
				frCount++
			}
			return true
		})
		return true
	})
	assert.Equal(t, 0, frCount, "orphan functionResponse must not reach Google after handover rewrite")
	assert.Equal(t, 1, len(contents.Array()), "functionResponse-only latest user drops; only summary remains")
}
