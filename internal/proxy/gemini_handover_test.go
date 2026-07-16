package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

type recordingSummarizerClient struct {
	calls      atomic.Int32
	lastBody   []byte
	respBody   string
	respStatus int
}

func (c *recordingSummarizerClient) Proxy(_ context.Context, _ router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	c.calls.Add(1)
	c.lastBody = append([]byte(nil), prep.Body...)
	status := c.respStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, c.respBody)
	return nil
}
func (c *recordingSummarizerClient) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return nil
}

func TestGeminiSwitch_RealProviderSummarizerBoundsHistory(t *testing.T) {
	t.Parallel()
	chunk := strings.Repeat("bbbb ", 8000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "systemInstruction":{"parts":[{"text":"be careful"}]},
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"functionCall":{"name":"edit","args":{"path":"a.go"}}}]},
    {"role":"user","parts":[{"functionResponse":{"name":"edit","response":{"result":"ok"}}}]},
    {"role":"user","parts":[{"text":"continue please"}]}
  ]
}`)
	sumClient := &recordingSummarizerClient{
		respBody: `{
  "id":"msg_test","type":"message","role":"assistant","model":"claude-haiku-4-5",
  "content":[{"type":"text","text":"Edited a.go successfully; user wants to continue."}],
  "usage":{"input_tokens":10,"output_tokens":8}
}`,
		respStatus: http.StatusOK,
	}
	ps := proxy.NewProviderSummarizer(sumClient, proxy.DefaultHandoverModel, time.Second)
	googleUp := &fakeProvider{}
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
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleUp},
		nil, false, nil, store, false,
		providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
	).WithSummarizer(ps)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	assert.Equal(t, "gemini-3.1-flash-lite-preview", rec.Header().Get(proxy.HeaderRouterModel))
	require.Equal(t, int32(1), sumClient.calls.Load(), "real ProviderSummarizer must reach Anthropic upstream")

	sumBody := sumClient.lastBody
	require.NotEmpty(t, sumBody)
	assert.Equal(t, "claude-haiku-4-5", gjson.GetBytes(sumBody, "model").String())
	assert.Equal(t, "be careful", gjson.GetBytes(sumBody, "system.0.text").String())
	assert.Contains(t, gjson.GetBytes(sumBody, "messages").Raw, `"tool_use"`)
	assert.Contains(t, gjson.GetBytes(sumBody, "messages").Raw, `"tool_result"`)
	assert.NotContains(t, string(sumBody), "functionCall", "Gemini wire must be converted before Anthropic upstream")

	require.NotEmpty(t, googleUp.proxyBodies)
	contents := gjson.GetBytes(googleUp.proxyBodies[0], "contents").Array()
	require.Len(t, contents, 2, "bounded [summary, latestUser]")
	assert.True(t, strings.HasPrefix(contents[0].Get("parts.0.text").String(), translate.HandoverSummaryTag))
	assert.Contains(t, contents[0].Get("parts.0.text").String(), "Edited a.go")
	assert.Equal(t, "continue please", contents[1].Get("parts.0.text").String())
	assert.NotContains(t, string(googleUp.proxyBodies[0]), "functionResponse")
	assert.NotContains(t, string(googleUp.proxyBodies[0]), chunk[:40])
}

func TestGeminiSwitch_SummarizerFailureKeepsFullHistory(t *testing.T) {
	t.Parallel()
	chunk := strings.Repeat("cccc ", 8000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"functionCall":{"name":"edit","args":{}}}]},
    {"role":"user","parts":[{"functionResponse":{"name":"edit","response":{"result":"ok"}}}]}
  ]
}`)
	sz := &fakeSummarizer{errOnCall: assert.AnError}
	googleUp := &fakeProvider{}
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
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleUp},
		nil, false, nil, store, false,
		providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
	).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	require.NotEmpty(t, googleUp.proxyBodies)
	raw := string(googleUp.proxyBodies[0])
	assert.Contains(t, raw, "functionCall", "failure must keep full history including functionCall")
	assert.Contains(t, raw, "functionResponse", "failure must keep full history including functionResponse")
	assert.NotContains(t, raw, translate.HandoverSummaryTag, "RewriteEnvelope must not run on summarizer failure")
	assert.Equal(t, int32(1), sz.calls.Load())
}

func TestGeminiSwitch_EmptySummaryKeepsFullHistory(t *testing.T) {
	t.Parallel()
	chunk := strings.Repeat("dddd ", 8000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"text":"ack"}]},
    {"role":"user","parts":[{"text":"continue"}]}
  ]
}`)
	sz := &fakeSummarizer{summary: ""} // success with empty text → same fallback as error
	googleUp := &fakeProvider{}
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
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleUp},
		nil, false, nil, store, false,
		providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
	).WithSummarizer(sz)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	require.NotEmpty(t, googleUp.proxyBodies)
	raw := string(googleUp.proxyBodies[0])
	assert.Contains(t, raw, chunk[:40], "empty summary must preserve full history")
	assert.NotContains(t, raw, translate.HandoverSummaryTag)
	assert.Equal(t, int32(1), sz.calls.Load())
	assert.Equal(t, "gemini-3.1-flash-lite-preview", rec.Header().Get(proxy.HeaderRouterModel))
}

func TestGeminiSwitch_ConcurrentRequestsRaceSafe(t *testing.T) {
	t.Parallel()
	chunk := strings.Repeat("eeee ", 4000)
	body := []byte(`{
  "model":"gemini-3.1-pro-preview",
  "contents":[
    {"role":"user","parts":[{"text":"` + chunk + `"}]},
    {"role":"model","parts":[{"text":"ack"}]},
    {"role":"user","parts":[{"text":"go"}]}
  ]
}`)
	const n = 16
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
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
			sz := &fakeSummarizer{summary: "race-safe summary"}
			googleUp := &fakeProvider{}
			svc := proxy.NewService(
				fr,
				map[string]providers.Client{providers.ProviderGoogle: googleUp},
				nil, false, nil, store, false,
				providers.ProviderGoogle, "gemini-3.1-flash-lite-preview", nil,
			).WithSummarizer(sz)
			ctx := authedCtx(uuid.New().String())
			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent", strings.NewReader(""))
			require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))
			require.NotEmpty(t, googleUp.proxyBodies)
			contents := gjson.GetBytes(googleUp.proxyBodies[0], "contents").Array()
			require.Len(t, contents, 2)
			assert.True(t, strings.HasPrefix(contents[0].Get("parts.0.text").String(), translate.HandoverSummaryTag))
		}()
	}
	for i := 0; i < n; i++ {
		<-done
	}
}
