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

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/proxy/usage"
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
	return proxy.NewService(&fakeRouter{decision: decision}, p, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)
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
	require.ErrorAs(t, err, &got, "must surface the typed UpstreamStatusError")
	assert.Equal(t, 400, got.Status)
}

// TestService_ProxyMessages_CrossFormatUpstreamErrorBodyReachesClient guards
// against the regression where a cross-format upstream non-2xx (e.g. OpenRouter
// 402 "out of credits") buffered the upstream body inside the
// AnthropicSSETranslator but never flushed it, because Finalize was skipped on
// any non-nil proxyErr. Claude Code would then see only the generic
// "upstream call failed" envelope written by the handler. The translated body
// must reach the client and the typed UpstreamStatusError must still surface
// to the handler so telemetry records the upstream status.
func TestService_ProxyMessages_CrossFormatUpstreamErrorBodyReachesClient(t *testing.T) {
	const upstreamBody = `{"error":{"message":"OpenRouter: insufficient credits","code":402,"type":"invalid_request_error"}}`
	provider := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, upstreamBody)
		},
		proxyErr: &providers.UpstreamStatusError{Status: http.StatusPaymentRequired},
	}
	svc := makeProxyService(
		router.Decision{Provider: providers.ProviderOpenRouter, Model: "deepseek/deepseek-chat"},
		map[string]providers.Client{providers.ProviderOpenRouter: provider},
	)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	err := svc.ProxyMessages(context.Background(), body, rec, httpReq)

	var got *providers.UpstreamStatusError
	require.ErrorAs(t, err, &got, "UpstreamStatusError must still propagate for telemetry")
	assert.Equal(t, http.StatusPaymentRequired, got.Status)

	assert.Equal(t, http.StatusPaymentRequired, rec.Code, "upstream status must reach the client")
	respBody := rec.Body.String()
	require.NotEmpty(t, respBody, "translated upstream error body must reach the client")
	assert.Contains(t, respBody, "insufficient credits", "upstream error message must survive translation")
	assert.Contains(t, respBody, `"type":"error"`, "body must be in Anthropic error envelope shape")
}

func TestService_ProxyMessages_EmbedOnlyUserMessageFlag(t *testing.T) {
	const firstUserPrompt = "Walk every Go file under router/internal/ and produce a one-paragraph summary of each."
	const secondUserPrompt = "Now narrow it to handlers under internal/api/."
	// CC-shaped body: system preamble + two user prompts separated by an
	// assistant turn + trailing tool_result. embedOnlyUserMessage must keep
	// both user prompts and drop the system text, the assistant tool_use, and
	// the tool_result blocks.
	body := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are Claude Code. CLAUDE.md says: do not use emojis...",
		"messages":[
			{"role":"user","content":"` + firstUserPrompt + `"},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"go.mod"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"module workweave/router\n\ngo 1.23\n\nrequire (\n\tgithub.com/gin-gonic/gin v1.10.0\n)"}]},
			{"role":"user","content":"` + secondUserPrompt + `"}
		]
	}`)

	t.Run("flag off uses concatenated stream", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr,
			map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
			nil,
			false,
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
		assert.Contains(t, got, "You are Claude Code", "flag=off keeps system prompt")
		assert.Contains(t, got, firstUserPrompt, "flag=off keeps first user message text")
	})

	t.Run("flag on concatenates user-role text and drops everything else", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr,
			map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
			nil,
			true,
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
		assert.Equal(t, firstUserPrompt+"\n"+secondUserPrompt, got,
			"flag=on emits user-role text only (no system, no assistant tool_use, no tool_result)")
	})
}

func TestService_ProxyMessages_EmbedOnlyUserMessageContextOverride(t *testing.T) {
	const userPrompt = "Find the race condition in main.go"
	body := []byte(`{
		"model":"claude-opus-4-7",
		"system":"You are Claude Code preamble...",
		"messages":[{"role":"user","content":"` + userPrompt + `"}]
	}`)

	cases := []struct {
		name           string
		startupFlag    bool
		ctxOverride    *bool
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
				nil,
				nil,
				false,
				"anthropic", "claude-haiku-4-5",
				nil,
			)

			ctx := context.Background()
			if tc.ctxOverride != nil {
				ctx = context.WithValue(ctx, proxy.EmbedOnlyUserMessageContextKey{}, *tc.ctxOverride)
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

// TestService_ProxyMessages_NoPinStoreRunsScorerEveryTurn verifies the
// pin-store-disabled path: without a pin store, every turn re-runs the
// cluster scorer (no Tier-3 legacy LRU short-circuits routing anymore).
func TestService_ProxyMessages_NoPinStoreRunsScorerEveryTurn(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5"}}
	svc := proxy.NewService(fr,
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		nil,
		nil, // pinStore disabled
		false,
		providers.ProviderAnthropic, "claude-haiku-4-5",
		nil,
	)

	ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-1")
	for range 2 {
		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))
	}
	assert.Equal(t, 2, fr.routeCalls, "without a pin store, both turns must consult the scorer")
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

	require.Len(t, provider.proxyBodies, 1)
	var translated map[string]any
	require.NoError(t, json.Unmarshal(provider.proxyBodies[0], &translated))
	assert.Equal(t, float64(100), translated["max_tokens"], "max_tokens preserved on translated body")
	msgs, _ := translated["messages"].([]any)
	require.Len(t, msgs, 1)

	var openAIOut map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &openAIOut))
	assert.Equal(t, "chat.completion", openAIOut["object"])
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

	require.ErrorIs(t, err, upstreamErr, "upstream Proxy error must propagate")
	assert.NotContains(t, rec.Body.String(), "translation failed",
		"Proxy error must not be masked by Finalize's translation failure body")
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
	assert.Equal(t, "gpt-4o", got["model"], "envelope rewrites model to decision.Model")
	msgs, _ := got["messages"].([]any)
	require.Len(t, msgs, 1)
	assert.Contains(t, rec.Body.String(), `"chat.completion"`)
}

// OpenRouter speaks OpenAI Chat Completions natively: an OpenAI-format
// inbound landing on an OpenRouter decision must take the no-translation path.
// Regression: eval harness v0.27 (OpenRouter OSS models) via mini-swe-agent's
// OpenAI client hit "no translation path defined".
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

	require.Len(t, provider.proxyBodies, 1)
	var got map[string]any
	require.NoError(t, json.Unmarshal(provider.proxyBodies[0], &got))
	assert.Equal(t, "qwen/qwen3-coder", got["model"], "envelope rewrites model to decision.Model")
	assert.Contains(t, rec.Body.String(), `"chat.completion"`)
}

// TestService_WithByokOnly_FiltersUnauthedProvidersFromScorer: with
// WithByokOnly(true), registered providers must not appear in EnabledProviders
// without per-request credentials, or argmax routes to the platform key and 402s.
func TestService_WithByokOnly_FiltersUnauthedProvidersFromScorer(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	providerMap := map[string]providers.Client{
		providers.ProviderAnthropic:  &fakeProvider{},
		providers.ProviderOpenRouter: &fakeProvider{},
	}

	t.Run("byok-off keeps every registered provider eligible (selfhost baseline)", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter)
	})

	t.Run("byok-on with no creds yields empty eligible set", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Empty(t, fr.capturedReq.EnabledProviders, "BYOK-only: registered providers ineligible without creds")
	})

	t.Run("byok-on Anthropic surface with x-api-key enables Anthropic only", func(t *testing.T) {
		// On the Anthropic surface, a client x-api-key is the legitimate
		// passthrough credential and enables Anthropic. It must NOT enable
		// OpenRouter or any other OpenAI-compat upstream — those use a
		// different inbound surface and reading the same header would be a
		// cross-provider credential leak.
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		httpReq.Header.Set("x-api-key", "sk-ant-customer-key")
		_ = svc.ProxyMessages(context.Background(), body, rec, httpReq)

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic,
			"client-supplied x-api-key on the Anthropic surface enables Anthropic")
		assert.NotContains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter,
			"client header on the Anthropic surface must not leak credentials into OpenAI-compat upstreams")
	})

	t.Run("byok-on Anthropic surface with inbound Bearer (e.g. Claude Code OAuth) enables nothing", func(t *testing.T) {
		// Regression for the 2026-05-13 prod incident: Claude Code passes an
		// Anthropic OAuth bearer (Authorization: Bearer sk-ant-oat-…) on every
		// request. The old code treated that header as creds for every
		// OpenAI-compat provider (OpenRouter/Fireworks/OpenAI/Google),
		// enabling argmax to pick OpenRouter and 401'ing at dispatch when no
		// OpenRouter key was attached. On the Anthropic surface a bearer is
		// never legitimate client creds (Anthropic uses x-api-key), so the
		// enabled set must remain empty.
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		httpReq.Header.Set("Authorization", "Bearer sk-ant-oat-claude-code-token")
		_ = svc.ProxyMessages(context.Background(), body, rec, httpReq)

		require.NotNil(t, fr.capturedReq)
		assert.NotContains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter,
			"inbound Bearer on the Anthropic surface must never enable OpenRouter")
		assert.NotContains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic,
			"a Bearer is not Anthropic auth — surface uses x-api-key")
	})
}

// Model exclusion flows from installation context or env override into
// the router.Request that the scorer consumes. Env override wins.
func TestService_ExcludedModelsThroughRequest(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)
	providerMap := map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}}

	t.Run("no override and no installation list → nil", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Nil(t, fr.capturedReq.ExcludedModels)
	})

	t.Run("installation list populates request", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

		ctx := context.WithValue(context.Background(), proxy.InstallationExcludedModelsContextKey{}, []string{"claude-opus-4-7", "gpt-5"})
		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.ExcludedModels, "claude-opus-4-7")
		assert.Contains(t, fr.capturedReq.ExcludedModels, "gpt-5")
	})

	t.Run("env override replaces installation list", func(t *testing.T) {
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithExcludedModelsOverride([]string{"gpt-4o"})

		// Installation list says one thing; override says another. Override wins.
		ctx := context.WithValue(context.Background(), proxy.InstallationExcludedModelsContextKey{}, []string{"claude-opus-4-7"})
		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.ExcludedModels, "gpt-4o")
		assert.NotContains(t, fr.capturedReq.ExcludedModels, "claude-opus-4-7")
		assert.True(t, svc.HasExcludedModelsOverride())
		assert.Equal(t, []string{"gpt-4o"}, svc.ExcludedModelsOverride())
	})
}

// TestService_ProxyMessages_UsageBypass_SkipsScorer guards the core
// contract of the rate-limit gate: while observed Anthropic utilization
// stays below threshold, ProxyMessages must NOT call the cluster
// scorer, and must dispatch to the Anthropic provider with the
// caller-requested model (not whatever the scorer would have picked).
func TestService_ProxyMessages_UsageBypass_SkipsScorer(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
	p := &fakeProvider{}
	obs := usage.NewObserver(0.95, 10*time.Minute, true)
	// Pin a sub-threshold reading under the credential we'll send in the
	// request header so the bypass branch engages on the first call.
	obs.RecordObservation(usage.CredentialKey([]byte("sk-ant-customer")), usage.Observation{FiveHourUtil: 0.2, WeeklyUtil: 0.3})

	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageBypass(obs)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-api-key", "sk-ant-customer")
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))
	assert.Equal(t, 0, fr.routeCalls, "scorer must not run while utilization is below threshold")
	require.Len(t, p.proxyBodies, 1, "request must be dispatched to Anthropic exactly once")
	assert.Contains(t, string(p.proxyBodies[0]), `"claude-opus-4-7"`, "bypass path must preserve the caller-requested model")
	assert.Equal(t, "usage_bypass", rec.Header().Get("x-router-decision"))
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"))
}

// TestService_ProxyMessages_UsageBypass_EngagesAtThreshold is the
// counterpart: once either window crosses threshold for this
// credential, the scorer must run. The fake scorer here routes to
// haiku, distinct from the opus the caller asked for.
func TestService_ProxyMessages_UsageBypass_EngagesAtThreshold(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
	p := &fakeProvider{}
	obs := usage.NewObserver(0.95, 10*time.Minute, true)
	obs.RecordObservation(usage.CredentialKey([]byte("sk-ant-hot")), usage.Observation{FiveHourUtil: 0.99, WeeklyUtil: 0.1})

	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageBypass(obs)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-api-key", "sk-ant-hot")
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))
	assert.Equal(t, 1, fr.routeCalls, "scorer must run once utilization crosses threshold")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"), "scorer's pick (haiku) replaces the requested opus")
}

// TestService_ProxyMessages_UsageBypass_ResolvesBYOKFromContext guards
// against the regression where byokCredentialsFromContext type-asserted
// the wrong shape: the auth middleware stores []*auth.ExternalAPIKey
// under ExternalAPIKeysContextKey{}, so the bypass gate must reach BYOK
// credentials via externalKeysFromContext + BuildCredentialsMap, not a
// direct map type-assertion. Regression: a router-authed BYOK request
// would silently fall back to attacker-controlled x-api-key headers.
func TestService_ProxyMessages_UsageBypass_ResolvesBYOKFromContext(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
	p := &fakeProvider{}
	obs := usage.NewObserver(0.95, 10*time.Minute, true)
	// Pin a near-limit reading under the BYOK credential (NOT under the
	// header value). If the gate keys off the BYOK map correctly, the
	// scorer must run. If it falls back to the header (the bug), the
	// gate sees no observation for that key and incorrectly bypasses.
	byokKey := []byte("sk-ant-byok-secret")
	obs.RecordObservation(usage.CredentialKey(byokKey), usage.Observation{FiveHourUtil: 0.99, WeeklyUtil: 0.1})

	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageBypass(obs)

	ctx := context.WithValue(context.Background(), proxy.ExternalAPIKeysContextKey{}, []*auth.ExternalAPIKey{
		{Provider: providers.ProviderAnthropic, Plaintext: byokKey},
	})
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-api-key", "sk-ant-attacker-supplied-headroom-key")
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, httpReq))
	assert.Equal(t, 1, fr.routeCalls, "BYOK credential's at-limit observation must engage routing — header-based fallback would falsely bypass")
}

// TestService_ProxyMessages_UsageBypass_RespectsModelExclusion guards
// the second security review: bypass must not let callers reach
// Anthropic models that installation policy has on its deny list.
// When the requested model is excluded, fall through to runTurnLoop so
// the tier clamp / scorer can substitute an allowed model.
func TestService_ProxyMessages_UsageBypass_RespectsModelExclusion(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
	p := &fakeProvider{}
	obs := usage.NewObserver(0.95, 10*time.Minute, true)
	obs.RecordObservation(usage.CredentialKey([]byte("sk-ant-customer")), usage.Observation{FiveHourUtil: 0.2, WeeklyUtil: 0.2})

	svc := proxy.NewService(fr, map[string]providers.Client{providers.ProviderAnthropic: p}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithUsageBypass(obs).
		WithExcludedModelsOverride([]string{"claude-opus-4-7"})

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-api-key", "sk-ant-customer")
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"hi"}]}`)

	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))
	assert.Equal(t, 1, fr.routeCalls, "excluded model must force the scorer to run even when utilization is below threshold")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"), "scorer must substitute an allowed model in place of the excluded opus")
}
