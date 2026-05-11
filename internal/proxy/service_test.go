package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRouter struct {
	decision    router.Decision
	err         error
	capturedReq *router.Request
	routeCalls  int
}

func (f *fakeRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	f.capturedReq = &req
	f.routeCalls++
	return f.decision, f.err
}

type fakeProvider struct {
	proxyBodies   [][]byte
	proxyResponse func(w http.ResponseWriter)
	proxyErr      error
}

func (f *fakeProvider) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	saved := make([]byte, len(prep.Body))
	copy(saved, prep.Body)
	f.proxyBodies = append(f.proxyBodies, saved)
	if f.proxyResponse != nil {
		f.proxyResponse(w)
	}
	return f.proxyErr
}

func (f *fakeProvider) Passthrough(ctx context.Context, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	return nil
}

func makeProxyService(decision router.Decision, p map[string]providers.Client) *proxy.Service {
	return proxy.NewService(&fakeRouter{decision: decision}, p, nil, false, 0, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)
}

func TestService_ProxyMessages_PropagatesUpstreamStatusError(t *testing.T) {
	upstreamErr := &providers.UpstreamStatusError{Status: 400}
	provider := &fakeProvider{proxyErr: upstreamErr}
	svc := makeProxyService(
		router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"},
		map[string]providers.Client{"anthropic": provider},
	)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(context.Background(), body, rec, httpReq)

	var got *providers.UpstreamStatusError
	require.ErrorAs(t, err, &got,
		"ProxyMessages must surface the typed UpstreamStatusError so "+
			"observability can log upstream_status alongside proxy_err")
	assert.Equal(t, 400, got.Status)
}

func TestService_ProxyMessages_EmbedLastUserMessageFlag(t *testing.T) {
	const userPrompt = "Walk every Go file under router/internal/ and produce a one-paragraph summary of each."
	// A realistic CC-shaped body: system preamble + an earlier user
	// prompt + a long tool_result the cluster scorer would otherwise
	// fingerprint on. The most recent user-authored text is the original
	// prompt, several messages back.
	body := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are Claude Code. CLAUDE.md says: do not use emojis...",
		"messages":[
			{"role":"user","content":"` + userPrompt + `"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"go.mod"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"module workweave/router\n\ngo 1.23\n\nrequire (\n\tgithub.com/gin-gonic/gin v1.10.0\n)"}]}
		]
	}`)

	t.Run("flag off uses concatenated stream", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr,
			map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
			nil,
			false,
			0,
			nil,
			nil,
			false,
			providers.ProviderAnthropic, "claude-haiku-4-5",
			nil,
		)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		got := fr.capturedReq.PromptText
		assert.Contains(t, got, "You are Claude Code",
			"flag=off must keep including the system prompt — that's the legacy concatenated-stream shape")
		assert.Contains(t, got, userPrompt,
			"flag=off must keep including each user message's text content")
	})

	t.Run("flag on uses last user message only", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr,
			map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
			nil,
			true,
			0,
			nil,
			nil,
			false,
			providers.ProviderAnthropic, "claude-haiku-4-5",
			nil,
		)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		got := fr.capturedReq.PromptText
		assert.Equal(t, userPrompt, got,
			"flag=on must hand the cluster scorer the most recent user-typed text verbatim, "+
				"with no system preamble or assistant content; that's the whole point of the flag")
	})
}

func TestService_ProxyMessages_EmbedLastUserMessageContextOverride(t *testing.T) {
	const userPrompt = "Find the race condition in main.go"
	body := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are Claude Code preamble...",
		"messages":[{"role":"user","content":"` + userPrompt + `"}]
	}`)

	cases := []struct {
		name           string
		startupFlag    bool
		ctxOverride    *bool // nil = no override
		wantPromptText string
	}{
		{
			name:           "ctx=true overrides startup=false",
			startupFlag:    false,
			ctxOverride:    boolPtr(true),
			wantPromptText: userPrompt,
		},
		{
			name:           "ctx=false overrides startup=true",
			startupFlag:    true,
			ctxOverride:    boolPtr(false),
			wantPromptText: "You are Claude Code preamble...\n" + userPrompt,
		},
		{
			name:           "no ctx override falls back to startup=true",
			startupFlag:    true,
			ctxOverride:    nil,
			wantPromptText: userPrompt,
		},
		{
			name:           "no ctx override falls back to startup=false",
			startupFlag:    false,
			ctxOverride:    nil,
			wantPromptText: "You are Claude Code preamble...\n" + userPrompt,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
			svc := proxy.NewService(fr,
				map[string]providers.Client{"anthropic": &fakeProvider{}},
				nil,
				tc.startupFlag,
				0,
				nil,
				nil,
				false,
				"anthropic", "claude-haiku-4-5",
				nil,
			)

			ctx := context.Background()
			if tc.ctxOverride != nil {
				ctx = context.WithValue(ctx, proxy.EmbedLastUserMessageContextKey{}, *tc.ctxOverride)
			}

			rec := httptest.NewRecorder()
			httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
			require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

			require.NotNil(t, fr.capturedReq)
			assert.Equal(t, tc.wantPromptText, fr.capturedReq.PromptText,
				"context override (%v) must beat startup flag (%v)", tc.ctxOverride, tc.startupFlag)
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestService_ProxyMessages_StickyBypassedByEvalOverrideHeaders(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	type runConfig struct {
		name           string
		header         string
		wantRouteCalls int
	}
	cases := []runConfig{
		{name: "no override → second call hits sticky cache", header: "", wantRouteCalls: 1},
		{name: "x-weave-cluster-version bypasses sticky", header: "x-weave-cluster-version", wantRouteCalls: 2},
		{name: "x-weave-embed-last-user-message bypasses sticky", header: "x-weave-embed-last-user-message", wantRouteCalls: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
			svc := proxy.NewService(fr,
				map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
				nil,
				false,
				time.Hour, // long TTL so sticky window stays open across both calls
				nil,
				nil,
				false,
				providers.ProviderAnthropic, "claude-haiku-4-5",
				nil,
			)

			ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-1")

			for i := 0; i < 2; i++ {
				rec := httptest.NewRecorder()
				httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
				if tc.header != "" {
					httpReq.Header.Set(tc.header, "true")
				}
				require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))
			}

			assert.Equal(t, tc.wantRouteCalls, fr.routeCalls,
				"router.Route call count must reflect whether sticky cache short-circuited the second call")
		})
	}
}

func TestService_ProxyOpenAIChatCompletion_AnthropicCrossFormat(t *testing.T) {
	anthropicResp := `{"id":"msg_abc","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"claude-opus-4-7","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`

	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(anthropicResp))
		},
	}
	svc := makeProxyService(
		router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "test"},
		map[string]providers.Client{"anthropic": provider},
	)

	openAIReq := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(openAIReq))

	err := svc.ProxyOpenAIChatCompletion(context.Background(), []byte(openAIReq), rec, httpReq)
	require.NoError(t, err)

	require.Len(t, provider.proxyBodies, 1, "provider.Proxy must be called exactly once")
	var translated map[string]any
	require.NoError(t, json.Unmarshal(provider.proxyBodies[0], &translated))
	assert.Equal(t, float64(100), translated["max_tokens"],
		"OpenAI max_tokens must be preserved on the translated Anthropic body")
	msgs, _ := translated["messages"].([]any)
	require.Len(t, msgs, 1, "translated Anthropic body must carry one user message")

	var openAIOut map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &openAIOut))
	assert.Equal(t, "chat.completion", openAIOut["object"],
		"response must be translated back to OpenAI chat.completion shape")
	choices, _ := openAIOut["choices"].([]any)
	require.Len(t, choices, 1)
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	assert.Equal(t, "Hello!", message["content"])
	assert.Equal(t, "stop", choice["finish_reason"])
}

func TestService_ProxyOpenAIChatCompletion_AnthropicProxyError_PropagatesError(t *testing.T) {
	upstreamErr := errors.New("dial tcp: connection refused")
	provider := &fakeProvider{
		proxyErr: upstreamErr,
	}
	svc := makeProxyService(
		router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "test"},
		map[string]providers.Client{"anthropic": provider},
	)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	err := svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, httpReq)

	require.ErrorIs(t, err, upstreamErr,
		"the upstream Proxy error must propagate so the handler can shape the correct OpenAI-format error")
	assert.NotContains(t, rec.Body.String(), "translation failed",
		"a Proxy error must not be masked by a synthesized 'translation failed' body from Finalize")
}

func TestService_ProxyOpenAIChatCompletion_NativeOpenAI(t *testing.T) {
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion"}`)
		},
	}
	svc := makeProxyService(
		router.Decision{Provider: "openai", Model: "gpt-4o", Reason: "test"},
		map[string]providers.Client{"openai": provider},
	)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	err := svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, httpReq)
	require.NoError(t, err)

	require.Len(t, provider.proxyBodies, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(provider.proxyBodies[0], &got))
	assert.Equal(t, "gpt-4o", got["model"], "envelope must rewrite model to decision.Model")
	msgs, _ := got["messages"].([]any)
	require.Len(t, msgs, 1, "user message must be preserved")
	assert.Contains(t, rec.Body.String(), `"chat.completion"`)
}

// OpenRouter speaks OpenAI Chat Completions natively. When an OpenAI-format
// inbound (e.g. mini-swe-agent / litellm) lands on an OpenRouter decision,
// the proxy must take the same no-translation path it does for native
// OpenAI — not error out with "no translation path defined". This regression
// surfaced when the eval harness ran v0.27 (which routes a chunk of traffic
// to OpenRouter-hosted OSS models) through mini-swe-agent's OpenAI client.
func TestService_ProxyOpenAIChatCompletion_NativeOpenRouter(t *testing.T) {
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion"}`)
		},
	}
	svc := makeProxyService(
		router.Decision{Provider: providers.ProviderOpenRouter, Model: "qwen/qwen3-coder", Reason: "test"},
		map[string]providers.Client{providers.ProviderOpenRouter: provider},
	)

	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))

	err := svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, httpReq)
	require.NoError(t, err)

	require.Len(t, provider.proxyBodies, 1, "provider.Proxy must be called once for the OpenRouter decision")
	var got map[string]any
	require.NoError(t, json.Unmarshal(provider.proxyBodies[0], &got))
	assert.Equal(t, "qwen/qwen3-coder", got["model"],
		"envelope must rewrite the inbound model to decision.Model so OpenRouter sees the routed pick")
	assert.Contains(t, rec.Body.String(), `"chat.completion"`,
		"OpenRouter response is already OpenAI-format and must pass through verbatim")
}

// TestService_WithByokOnly_FiltersUnauthedProvidersFromScorer asserts the
// managed-deployment guard: when WithByokOnly(true) is set, a registered
// provider must NOT appear in req.EnabledProviders unless the caller
// supplied BYOK credentials or a client-supplied Bearer/x-api-key for that
// provider. Without this guard, the cluster scorer happily picks
// e.g. OpenRouter for an installation with no OpenRouter key and the
// upstream call 402s on the platform's exhausted deployment key —
// exactly the regression this flag fixes.
func TestService_WithByokOnly_FiltersUnauthedProvidersFromScorer(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	providerMap := map[string]providers.Client{
		providers.ProviderAnthropic:  &fakeProvider{},
		providers.ProviderOpenRouter: &fakeProvider{},
	}

	t.Run("byok-off keeps every registered provider eligible (selfhost baseline)", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, 0, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter,
			"selfhost default: registered providers are eligible without per-request BYOK")
	})

	t.Run("byok-on with no creds yields empty eligible set", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, 0, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Empty(t, fr.capturedReq.EnabledProviders,
			"managed/BYOK-only: a registered provider must not be eligible without BYOK or client-supplied creds")
	})

	t.Run("byok-on with client-supplied Bearer enables only that provider", func(t *testing.T) {
		// Anthropic decision so the proxy completes; this subtest only asserts
		// on req.EnabledProviders captured at Route() time.
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, 0, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		httpReq.Header.Set("Authorization", "Bearer sk-or-v1-customer-key")
		_ = svc.ProxyMessages(context.Background(), body, rec, httpReq)

		require.NotNil(t, fr.capturedReq)
		assert.NotContains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic,
			"managed/BYOK-only: Anthropic must not be eligible without anthropic-specific creds")
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter,
			"managed/BYOK-only: a client-supplied Bearer for OpenRouter must make it eligible")
	})
}
