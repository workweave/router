package proxy_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/anthropic"
	"workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anthropicMessageJSON is a minimal Anthropic Messages body for OpenAI→Anthropic translation.
const anthropicMessageJSON = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`

// TestProxyOpenAI_OSSOutageFailsOverToBaselineAnthropic: OSS outage → Anthropic baseline on the OpenAI wire.
func TestProxyOpenAI_OSSOutageFailsOverToBaselineAnthropic(t *testing.T) {
	var (
		mu                     sync.Mutex
		fireworksCount         int
		openRouterCount        int
		anthropicCount         int
		anthropicReceivedModel string
	)

	fail503 := func(counter *int) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*counter++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
		}
	}
	fireworks := httptest.NewServer(fail503(&fireworksCount))
	defer fireworks.Close()
	openrouter := httptest.NewServer(fail503(&openRouterCount))
	defer openrouter.Close()

	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		anthropicCount++
		anthropicReceivedModel = gjson.GetBytes(body, "model").String()
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicMessageJSON))
	}))
	defer anthropicUpstream.Close()

	store := newFakePinStore()
	tel := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("test-fw-key", fireworks.URL),
			"openrouter": openaicompat.NewClient("test-or-key", openrouter.URL),
			"anthropic":  anthropic.NewClient("test-anthropic-key", anthropicUpstream.URL),
		},
		nil, false, nil, store, false, providers.ProviderAnthropic, "claude-haiku-4-5", tel,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks":  {},
		"openrouter": {},
		"anthropic":  {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyOpenAIChatCompletion(authedCtx("11111111-1111-1111-1111-111111111111"), body, rec, req)
	require.NoError(t, err, "ProxyOpenAIChatCompletion should succeed via baseline failover to Anthropic")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, fireworksCount, "Fireworks (primary OSS binding) tried once")
	assert.Equal(t, 1, openRouterCount, "OpenRouter (OSS fallback binding) tried once")
	assert.Equal(t, 1, anthropicCount, "Anthropic baseline failover dispatched once")
	assert.Equal(t, "claude-opus-4-8", anthropicReceivedModel, "baseline failover must request the caller's model on Anthropic")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "anthropic", rec.Header().Get(proxy.HeaderRouterProvider), "served provider header reflects the baseline failover")
	assert.Equal(t, "claude-opus-4-8", rec.Header().Get(proxy.HeaderRouterModel), "x-router-model reflects the baseline model that served")
	assert.Contains(t, rec.Body.String(), `"object":"chat.completion"`)
	assert.NotContains(t, rec.Body.String(), "deepseek/deepseek-v4-pro")

	require.NotEmpty(t, store.usages, "baseline failover must write pin usage")
	assert.Equal(t, "claude-opus-4-8", store.usages[len(store.usages)-1].ServedModel, "pin usage records the served baseline model")
}

// TestProxyOpenAI_ForcedModelUnavailableDoesNotSubstituteAnthropic: forced-model must not substitute Anthropic.
func TestProxyOpenAI_ForcedModelUnavailableDoesNotSubstituteAnthropic(t *testing.T) {
	var anthropicCount int
	var googleCount int
	anthropicProv := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		anthropicCount++
		w.WriteHeader(http.StatusOK)
	}}
	google := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		googleCount++
		w.WriteHeader(http.StatusOK)
	}}
	svc := makeProxyService(
		router.Decision{
			Provider: providers.ProviderGoogle,
			Model:    "gemini-3.1-pro-preview",
			Reason:   translate.ReasonUserForceModel,
		},
		map[string]providers.Client{
			providers.ProviderAnthropic: anthropicProv,
			providers.ProviderGoogle:    google,
		},
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyOpenAIChatCompletion(context.Background(), body, rec, req)
	require.Error(t, err)
	assert.Equal(t, 0, anthropicCount, "forced model requests must never substitute Anthropic")
	assert.Equal(t, 0, googleCount, "unwired forced provider must not be dispatched")
}

// TestProxyOpenAI_OSSOutageNoBaselineWhenRequestedModelIsOSS: no baseline when the caller requested OSS.
func TestProxyOpenAI_OSSOutageNoBaselineWhenRequestedModelIsOSS(t *testing.T) {
	var anthropicCount int
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer anthropicUpstream.Close()

	fail := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}
	fireworks := httptest.NewServer(http.HandlerFunc(fail))
	defer fireworks.Close()
	openrouter := httptest.NewServer(http.HandlerFunc(fail))
	defer openrouter.Close()

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("k", fireworks.URL),
			"openrouter": openaicompat.NewClient("k", openrouter.URL),
			"anthropic":  anthropic.NewClient("k", anthropicUpstream.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks": {}, "openrouter": {}, "anthropic": {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	body := []byte(`{"model":"deepseek/deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyOpenAIChatCompletion(context.Background(), body, rec, req)
	assert.Equal(t, 0, anthropicCount, "baseline failover must not fire when the caller requested the OSS model")
}

// TestProxyOpenAI_OSSOutageNoBaselineWhenAnthropicExcluded: no baseline when Anthropic is excluded.
func TestProxyOpenAI_OSSOutageNoBaselineWhenAnthropicExcluded(t *testing.T) {
	var anthropicCount int
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer anthropicUpstream.Close()

	fail := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
	}
	fireworks := httptest.NewServer(http.HandlerFunc(fail))
	defer fireworks.Close()
	openrouter := httptest.NewServer(http.HandlerFunc(fail))
	defer openrouter.Close()

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("k", fireworks.URL),
			"openrouter": openaicompat.NewClient("k", openrouter.URL),
			"anthropic":  anthropic.NewClient("k", anthropicUpstream.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks": {}, "openrouter": {}, "anthropic": {},
	}).WithExcludedProvidersOverride([]string{providers.ProviderAnthropic})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyOpenAIChatCompletion(context.Background(), body, rec, req)
	assert.Equal(t, 0, anthropicCount, "baseline failover must not hit Anthropic when it is excluded")
	assert.NotEqual(t, providers.ProviderAnthropic, rec.Header().Get(proxy.HeaderRouterProvider), "served provider must not be the excluded Anthropic")
}
