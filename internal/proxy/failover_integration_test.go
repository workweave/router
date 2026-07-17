package proxy_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/openai"
	"workweave/router/internal/providers/openaicompat"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProxyMessages_FireworksFailureFallbackToOpenRouter wires the real
// dispatch + translator + openaicompat clients against stub upstreams
// and asserts that:
//   - On Fireworks 503, the dispatch falls over to OpenRouter cleanly.
//   - The client receives a valid Anthropic SSE stream (the failover is
//     invisible at the wire-format layer).
//   - The x-router-fallback-from header surfaces the primary.
//   - The OpenRouter request body has the OpenRouter-specific gates that
//     Prepare* would only emit when opts.TargetProvider = openrouter
//     (provider hint, reasoning: {enabled:false}) — proves per-attempt
//     prep rebuild is wired correctly.
func TestProxyMessages_FireworksFailureFallbackToOpenRouter(t *testing.T) {
	var (
		mu                     sync.Mutex
		openRouterReceivedBody []byte
		fireworksRequestCount  int
		openRouterRequestCount int
	)

	fireworks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fireworksRequestCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"fireworks edge unavailable"}}`))
	}))
	defer fireworks.Close()

	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		openRouterReceivedBody = body
		openRouterRequestCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Minimal OpenAI SSE response: one chunk with text + stop.
		chunks := []string{
			`data: {"id":"or-1","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-pro","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"or-1","object":"chat.completion.chunk","created":1,"model":"deepseek/deepseek-v4-pro","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer openrouter.Close()

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("test-fw-key", fireworks.URL),
			"openrouter": openaicompat.NewClient("test-or-key", openrouter.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks":  {},
		"openrouter": {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"deepseek/deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(context.Background(), body, rec, req)
	require.NoError(t, err, "ProxyMessages should succeed after failover to OpenRouter")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, fireworksRequestCount, "Fireworks called exactly once")
	assert.Equal(t, 1, openRouterRequestCount, "OpenRouter called exactly once (failover)")

	// Client sees valid Anthropic SSE.
	respBody := rec.Body.String()
	assert.Contains(t, respBody, "event: message_start", "Anthropic stream should start with message_start")
	assert.Contains(t, respBody, "event: message_stop", "Anthropic stream should end with message_stop")

	// Fallback headers surface the primary that failed.
	assert.Equal(t, "fireworks", rec.Header().Get(proxy.HeaderRouterFallbackFrom))
	assert.Equal(t, "1", rec.Header().Get(proxy.HeaderRouterFallbackAttempt))

	// Per-attempt prep verification: OpenRouter received a body that
	// includes the OpenRouter-only gates from emit_openai.go. Without the
	// per-attempt rebuild, the body would carry Fireworks-shape (no
	// provider hint, reasoning enabled by default).
	require.NotEmpty(t, openRouterReceivedBody, "OpenRouter should have received a request body")
	var got map[string]any
	require.NoError(t, json.Unmarshal(openRouterReceivedBody, &got))

	provider, ok := got["provider"].(map[string]any)
	require.True(t, ok, "OpenRouter request must carry the `provider` hint for deepseek/* (got: %s)", string(openRouterReceivedBody))
	order, _ := provider["order"].([]any)
	require.NotEmpty(t, order, "provider.order must be set")
	assert.Equal(t, "deepseek", order[0])

	reasoning, ok := got["reasoning"].(map[string]any)
	require.True(t, ok, "OpenRouter request must carry the `reasoning` hint for deepseek/*")
	assert.Equal(t, false, reasoning["enabled"], "reasoning.enabled must be false to avoid burning max_tokens on hidden thinking")
}

// TestProxyMessages_BothBindingsFail asserts the format-specific
// exhaustion renderer: when every binding returns an error, the
// Anthropic client sees the upstream error envelope translated to
// Anthropic shape via translate.OpenAIToAnthropicError, NOT the raw
// upstream JSON.
func TestProxyMessages_BothBindingsFail(t *testing.T) {
	fireworks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"fireworks down"}}`))
	}))
	defer fireworks.Close()

	openrouter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"openrouter also down","type":"upstream_error"}}`))
	}))
	defer openrouter.Close()

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: "fireworks", Model: "deepseek/deepseek-v4-pro"}},
		map[string]providers.Client{
			"fireworks":  openaicompat.NewClient("test-fw-key", fireworks.URL),
			"openrouter": openaicompat.NewClient("test-or-key", openrouter.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		"fireworks":  {},
		"openrouter": {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"deepseek/deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyMessages(context.Background(), body, rec, req)

	// Client sees the FINAL (OpenRouter's) status code and a translated
	// Anthropic-shape error envelope, not the raw OpenAI shape.
	assert.Equal(t, http.StatusBadGateway, rec.Code, "exhaustion surfaces the last attempt's status")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"), "exhaustion writes a JSON HTTP response, not SSE")

	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "body must be JSON")
	assert.Equal(t, "error", got["type"], "Anthropic-shape error envelope: top-level `type` == \"error\"")
	innerErr, ok := got["error"].(map[string]any)
	require.True(t, ok, "Anthropic-shape error envelope: `error` is a nested object")
	assert.Contains(t, innerErr["message"], "openrouter also down")
}

// TestProxyMessages_SingleBindingPreservesEagerPrelude asserts that
// single-binding requests (every Anthropic-native model today) still
// fire translator.Prelude eagerly to the client writer — preserving
// main #220's TTFB win. The preludeBuffer is not engaged because
// resolveBindingsForDispatch returns a single-element slice.
func TestProxyMessages_SingleBindingPreservesEagerPrelude(t *testing.T) {
	// An Anthropic-shape upstream that emits SSE chunks. We don't assert
	// the chunks here; we assert that the response is committed (200) and
	// the client sees message_start before message_stop — i.e. the
	// translator's Prelude wasn't swallowed by an inadvertent buffer.
	anth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Minimal valid Anthropic-shape stream.
		for _, c := range []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-haiku-4-5\"}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		} {
			_, _ = w.Write([]byte(c))
		}
	}))
	defer anth.Close()

	// claude-haiku-4-5 is single-binding (Anthropic only).
	svc := makeProxyService(
		router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"},
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{
				proxyResponse: func(w http.ResponseWriter) {
					// Mirror the translator's expected SSE shape.
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_x\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-haiku-4-5\"}}\n\n"))
					_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
				},
			},
		},
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-haiku-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, req))
	assert.Equal(t, http.StatusOK, rec.Code)
	respBody := rec.Body.String()
	assert.Contains(t, respBody, "message_start")
	assert.Contains(t, respBody, "message_stop")
	// No fallback header for single-binding requests.
	assert.Empty(t, rec.Header().Get(proxy.HeaderRouterFallbackFrom))
}

// TestProxyMessages_SingleBindingStreamingPreCommitError asserts the fixed
// behavior: when a single-binding cross-format streaming request gets an
// upstream error BEFORE any upstream byte arrives, the preludeBuffer
// discards the buffered prelude and the client receives a clean
// Anthropic-shape JSON error envelope at the upstream's status — not a
// stranded `message_start` text-only turn that Claude Code would reject
// for missing tool_use. This is the v0.58 SWE-bench bake-off regression
// fix.
func TestProxyMessages_SingleBindingStreamingPreCommitError(t *testing.T) {
	// Stub upstream OpenAI-compat provider that 503s on every request.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable","type":"upstream_error"}}`))
	}))
	defer stub.Close()

	// gpt-5 is single-binding to openai in catalog; route there from an
	// inbound Anthropic Messages request so the cross-format
	// AnthropicSSETranslator + Prelude path runs.
	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5"}},
		map[string]providers.Client{
			providers.ProviderOpenAI: openaicompat.NewClient("test-key", stub.URL),
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderOpenAI: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	_ = svc.ProxyMessages(context.Background(), body, rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"pre-commit upstream error surfaces upstream's status, not a stranded HTTP 200")
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"),
		"pre-commit error is a clean JSON envelope, not a half-emitted SSE stream")

	respBody := rec.Body.String()
	assert.NotContains(t, respBody, "event: message_start",
		"prelude bytes were buffered and discarded — no stranded marker on the wire")
	assert.NotContains(t, respBody, "✦ **Weave Router**",
		"routing marker discarded with the prelude buffer")
	assert.Contains(t, respBody, `"type":"error"`, "Anthropic-shape error envelope")
	assert.Contains(t, respBody, "upstream unavailable", "translated upstream message reaches the client")
}

func TestProxyMessages_ResponsesFailureBeforeOutputFallsBackToBaseline(t *testing.T) {
	var (
		mu          sync.Mutex
		openAICalls int
	)
	openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		openAICalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.failed\n"+
			`data: {"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"server_error","message":"request rejected before output"},"output":[]}}`+"\n\n")
	}))
	defer openAIUpstream.Close()

	baseline := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_baseline\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-8\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recovered\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}}

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.5", Reason: "test"}},
		map[string]providers.Client{
			providers.ProviderOpenAI:    openai.NewClient("test-key", openAIUpstream.URL),
			providers.ProviderAnthropic: baseline,
		},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{
		providers.ProviderOpenAI:    {},
		providers.ProviderAnthropic: {},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-8","stream":true,"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],"messages":[{"role":"user","content":"inspect this"}]}`)

	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, req))
	mu.Lock()
	assert.GreaterOrEqual(t, openAICalls, 1)
	mu.Unlock()
	require.Len(t, baseline.proxyBodies, 1, "pre-output Responses failure must retry on the requested Anthropic model")
	assert.Contains(t, rec.Body.String(), "recovered")
	assert.Contains(t, rec.Body.String(), "event: message_stop")
	assert.NotContains(t, rec.Body.String(), "event: error")
}

// sequencedGeminiClient is a providers.Client that returns a scripted result
// per call (and captures the prepared body each time) so a test can assert the
// router re-emitted a different body on retry.
type sequencedGeminiClient struct {
	mu        sync.Mutex
	bodies    [][]byte
	responses []func(w http.ResponseWriter) error
}

func (c *sequencedGeminiClient) Proxy(_ context.Context, _ router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	c.mu.Lock()
	i := len(c.bodies)
	c.bodies = append(c.bodies, append([]byte(nil), prep.Body...))
	c.mu.Unlock()
	if i < len(c.responses) {
		return c.responses[i](w)
	}
	return nil
}

func (c *sequencedGeminiClient) Passthrough(_ context.Context, _ providers.PreparedRequest, _ http.ResponseWriter, _ *http.Request) error {
	return nil
}

// TestProxyMessages_GeminiValidated400RetriesWithAuto reproduces Jerry's
// "Request contains an invalid argument" session: a tools-with-no-forced-choice
// Gemini 3.x turn goes out under functionCallingConfig.mode=VALIDATED, Gemini
// can't compile a tool schema into its decode grammar and 400s the whole
// request pre-commit, and the router rescues it by re-emitting the SAME tools
// under mode=AUTO. Asserts both attempts fire, the second carries AUTO, and the
// client sees a clean Anthropic stream rather than the upstream 400.
func TestProxyMessages_GeminiValidated400RetriesWithAuto(t *testing.T) {
	geminiSSE := `data: {"candidates":[{"content":{"parts":[{"text":"I am an AI assistant."}],"role":"model"},"index":0}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"totalTokenCount":9}}` + "\n\n"

	client := &sequencedGeminiClient{
		responses: []func(w http.ResponseWriter) error{
			// Call 1: VALIDATED-mode INVALID_ARGUMENT, pre-commit (no write).
			func(http.ResponseWriter) error {
				return &providers.UpstreamStatusError{Status: http.StatusBadRequest}
			},
			// Call 2: AUTO mode compiles fine and streams a valid response.
			func(w http.ResponseWriter) error {
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, geminiSSE)
				return nil
			},
		},
	}

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-3.1-pro-preview"}},
		map[string]providers.Client{providers.ProviderGoogle: client},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderGoogle: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"gemini-3.1-pro-preview","stream":true,` +
		`"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],` +
		`"messages":[{"role":"user","content":"who are you"}]}`)

	err := svc.ProxyMessages(context.Background(), body, rec, req)
	require.NoError(t, err, "the AUTO-mode retry must rescue a VALIDATED-mode 400")

	require.Len(t, client.bodies, 2, "first VALIDATED attempt 400s, second AUTO attempt runs")
	assert.Contains(t, string(client.bodies[0]), `"mode":"VALIDATED"`, "first attempt requested VALIDATED decoding")
	assert.Contains(t, string(client.bodies[1]), `"mode":"AUTO"`, "the retry downgraded the tool mode to AUTO")
	assert.NotContains(t, string(client.bodies[1]), `"mode":"VALIDATED"`)

	respBody := rec.Body.String()
	assert.Contains(t, respBody, "event: message_start", "client sees the rescued Anthropic stream")
	assert.Contains(t, respBody, "event: message_stop")
}

// TestProxyMessages_GeminiNon400NotRetried guards the gate: a non-400 Gemini
// error (e.g. 503) must NOT trigger the AUTO downgrade — that path is reserved
// for VALIDATED-mode schema-grammar rejections, and re-emitting would waste an
// upstream call.
func TestProxyMessages_GeminiNon400NotRetried(t *testing.T) {
	client := &sequencedGeminiClient{
		responses: []func(w http.ResponseWriter) error{
			func(http.ResponseWriter) error {
				return &providers.UpstreamStatusError{Status: http.StatusServiceUnavailable}
			},
		},
	}

	svc := proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: providers.ProviderGoogle, Model: "gemini-3.1-pro-preview"}},
		map[string]providers.Client{providers.ProviderGoogle: client},
		nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderGoogle: {}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"gemini-3.1-pro-preview","stream":true,` +
		`"tools":[{"name":"Read","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],` +
		`"messages":[{"role":"user","content":"who are you"}]}`)

	_ = svc.ProxyMessages(context.Background(), body, rec, req)

	assert.Len(t, client.bodies, 1, "a 503 is not a VALIDATED-schema 400 — no AUTO retry")
}
