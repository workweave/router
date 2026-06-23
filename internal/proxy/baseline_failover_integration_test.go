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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anthropicMessageSSE is a minimal but well-formed Anthropic Messages stream
// (message_start … message_stop) for stubbing the native Anthropic upstream.
const anthropicMessageSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-8\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// TestProxyMessages_OSSOutageFailsOverToBaselineAnthropic wires the real
// dispatch path against stub upstreams and asserts the in-turn baseline
// failover: when the router cost-routes a `claude-opus-4-8` request to an OSS
// model (deepseek/deepseek-v4-pro) and every OSS binding fails (the Redwood
// demo's Fireworks-503 → OpenRouter-down double outage), the turn re-dispatches
// the requested model on Anthropic instead of surfacing "model may not exist".
func TestProxyMessages_OSSOutageFailsOverToBaselineAnthropic(t *testing.T) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(anthropicMessageSSE))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer anthropicUpstream.Close()

	store := newFakePinStore()
	// A telemetry sink makes usageRequired() true so the usage extractor runs
	// and recordTurnUsage fires with the served-turn token counts.
	tel := newCaptureTelemetry()
	svc := proxy.NewService(
		// Router cost-routes the opus request to deepseek on Fireworks.
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
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// The caller asked for claude-opus-4-8; the router's cost decision sent it
	// to deepseek. The baseline-failover target is the requested model.
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(authedCtx("11111111-1111-1111-1111-111111111111"), body, rec, req)
	require.NoError(t, err, "ProxyMessages should succeed via baseline failover to Anthropic")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, fireworksCount, "Fireworks (primary OSS binding) tried once")
	assert.Equal(t, 1, openRouterCount, "OpenRouter (OSS fallback binding) tried once")
	assert.Equal(t, 1, anthropicCount, "Anthropic baseline failover dispatched once")
	assert.Equal(t, "claude-opus-4-8", anthropicReceivedModel, "baseline failover must request the caller's model on Anthropic")

	respBody := rec.Body.String()
	assert.Contains(t, respBody, "event: message_start", "client sees the Anthropic stream start")
	assert.Contains(t, respBody, "event: message_stop", "client sees the Anthropic stream end")
	assert.Equal(t, "anthropic", rec.Header().Get(proxy.HeaderRouterProvider), "served provider header reflects the baseline failover")
	// The routing marker text and x-router-model header must name the baseline
	// model that served, not the cost-routed OSS slug that went dark.
	assert.Equal(t, "claude-opus-4-8", rec.Header().Get(proxy.HeaderRouterModel), "x-router-model reflects the baseline model that served")
	assert.Contains(t, respBody, "claude-opus-4-8", "routing marker names the served baseline model")
	assert.NotContains(t, respBody, "deepseek/deepseek-v4-pro", "marker must not name the OSS model that never completed")

	// The session pin must record the baseline model that actually served, not
	// the cost-routed OSS id — otherwise next-turn switch detection is wrong.
	require.NotEmpty(t, store.usages, "baseline failover must write pin usage")
	assert.Equal(t, "claude-opus-4-8", store.usages[len(store.usages)-1].ServedModel, "pin usage records the served baseline model")
}

// TestProxyMessages_OSSOutageNoBaselineWhenRequestedModelIsOSS asserts the
// guard: when the caller themselves requested the OSS model (so the baseline
// equals the routed model), exhaustion surfaces the upstream error envelope —
// baseline failover does NOT fire and does not mask the real failure.
func TestProxyMessages_OSSOutageNoBaselineWhenRequestedModelIsOSS(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// Caller requested the OSS model directly: baseline == routed model.
	body := []byte(`{"model":"deepseek/deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyMessages(context.Background(), body, rec, req)
	assert.Equal(t, 0, anthropicCount, "baseline failover must not fire when the caller requested the OSS model")
}

// TestProxyMessages_OSSOutageNoBaselineWhenAnthropicExcluded asserts the
// provider-exclusion contract: when the installation excludes Anthropic,
// baseline failover must NOT defer the OSS error to an Anthropic retry it can
// never run — the original upstream error surfaces and Anthropic is never hit.
func TestProxyMessages_OSSOutageNoBaselineWhenAnthropicExcluded(t *testing.T) {
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
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyMessages(context.Background(), body, rec, req)
	assert.Equal(t, 0, anthropicCount, "baseline failover must not hit Anthropic when it is excluded")
	assert.NotEqual(t, providers.ProviderAnthropic, rec.Header().Get(proxy.HeaderRouterProvider), "served provider must not be the excluded Anthropic")
}

// TestProxyMessages_FailedBaselineReportsAnthropicProvider asserts that when
// both the OSS bindings AND the Anthropic baseline retry fail, the telemetry
// row pairs the baseline (Anthropic) model with the Anthropic provider — not
// the OSS primary that never served that model.
func TestProxyMessages_FailedBaselineReportsAnthropicProvider(t *testing.T) {
	fail := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}
	fireworks := httptest.NewServer(http.HandlerFunc(fail))
	defer fireworks.Close()
	openrouter := httptest.NewServer(http.HandlerFunc(fail))
	defer openrouter.Close()
	// The Anthropic baseline retry also fails (503), so winnerIdx stays -1.
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(fail))
	defer anthropicUpstream.Close()

	tel := newCaptureTelemetry()
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("k", fireworks.URL),
			"openrouter": openaicompat.NewClient("k", openrouter.URL),
			"anthropic":  anthropic.NewClient("k", anthropicUpstream.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", tel,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks": {}, "openrouter": {}, "anthropic": {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	// A non-nil installation ID gates the telemetry write.
	ctx := context.WithValue(context.Background(), proxy.InstallationIDContextKey{}, "11111111-1111-1111-1111-111111111111")
	_ = svc.ProxyMessages(ctx, body, rec, req)

	row := tel.firstRow(t)
	assert.Equal(t, "claude-opus-4-8", row.DecisionModel, "telemetry records the baseline model after failover")
	assert.Equal(t, providers.ProviderAnthropic, row.DecisionProvider, "failed-baseline provider must match the baseline model, not the OSS primary")
}
