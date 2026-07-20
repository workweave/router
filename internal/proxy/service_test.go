package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRouter struct {
	decision    router.Decision
	err         error
	capturedReq *router.Request
	routeCalls  int
}

type fakePreviewRouter struct {
	previewResult policy.PreviewResult
	previewReq    *router.Request
	previewCalls  int
	routeCalls    int
}

func (f *fakePreviewRouter) Route(context.Context, router.Request) (router.Decision, error) {
	f.routeCalls++
	return router.Decision{}, errors.New("serving route must not run during preview")
}

func (f *fakePreviewRouter) PreviewRoute(_ context.Context, req router.Request) (policy.PreviewResult, error) {
	f.previewCalls++
	f.previewReq = &req
	return f.previewResult, nil
}

func (f *fakeRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	f.capturedReq = &req
	f.routeCalls++
	return f.decision, f.err
}

type fakeProvider struct {
	proxyBodies    [][]byte
	proxyEndpoints []providers.Endpoint
	proxyResponse  func(w http.ResponseWriter)
	proxyErr       error
	// proxyCreds records the resolved credential per dispatch; nil means
	// deployment-key fallback (no credential set).
	proxyCreds []*proxy.Credentials
}

func (f *fakeProvider) Proxy(ctx context.Context, decision router.Decision, prep providers.PreparedRequest, w http.ResponseWriter, r *http.Request) error {
	saved := make([]byte, len(prep.Body))
	copy(saved, prep.Body)
	f.proxyBodies = append(f.proxyBodies, saved)
	f.proxyEndpoints = append(f.proxyEndpoints, prep.Endpoint)
	f.proxyCreds = append(f.proxyCreds, proxy.CredentialsFromContext(ctx))
	if f.proxyResponse != nil {
		f.proxyResponse(w)
	}
	return f.proxyErr
}

func TestService_PreviewAnthropicRouteBuildsServingCandidateContextWithoutDispatch(t *testing.T) {
	anthropicProvider := &fakeProvider{}
	openAIProvider := &fakeProvider{}
	previewer := &fakePreviewRouter{previewResult: policy.PreviewResult{
		SchemaVersion:     policy.SchemaVersionV1,
		EligibleRosterIDs: []string{"anthropic/claude-opus-4-8", "openai/gpt-5.5"},
	}}
	svc := proxy.NewService(&fakeRouter{}, map[string]providers.Client{
		providers.ProviderAnthropic: anthropicProvider,
		providers.ProviderOpenAI:    openAIProvider,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithPolicyStrategy(policy.StrategySpec{Strategy: router.StrategyHMM, Router: previewer}).
		WithDeploymentKeyedProviders(map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		})

	ctx := router.WithStrategy(context.Background(), router.StrategyHMM)
	ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, "org-1")
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, "1791da5d-d0db-494c-8574-859a4cb20d97")
	ctx = context.WithValue(ctx, proxy.InstallationExcludedModelsContextKey{}, []string{"gpt-5.5-mini"})
	ctx = context.WithValue(ctx, proxy.InstallationPreferredModelsContextKey{}, []string{"claude-opus-4-8"})
	body := []byte(`{"model":"claude-opus-4-8[1m]","messages":[{"role":"user","content":"inspect this repository"}],"max_tokens":4096,"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}}]}`)

	result, err := svc.PreviewAnthropicRoute(ctx, body, http.Header{})

	require.NoError(t, err)
	assert.Equal(t, previewer.previewResult, result)
	assert.Equal(t, 1, previewer.previewCalls)
	assert.Zero(t, previewer.routeCalls)
	require.NotNil(t, previewer.previewReq)
	assert.Equal(t, "claude-opus-4-8", previewer.previewReq.RequestedModel)
	assert.Equal(t, "org-1", previewer.previewReq.OrganizationID)
	assert.Equal(t, "1791da5d-d0db-494c-8574-859a4cb20d97", previewer.previewReq.InstallationID)
	assert.True(t, previewer.previewReq.HasTools)
	assert.Equal(t, []string{"Read"}, previewer.previewReq.AvailableTools)
	assert.Contains(t, previewer.previewReq.EnabledProviders, providers.ProviderAnthropic)
	assert.Contains(t, previewer.previewReq.EnabledProviders, providers.ProviderOpenAI)
	assert.Contains(t, previewer.previewReq.ExcludedModels, "gpt-5.5-mini")
	assert.Equal(t, []string{"claude-opus-4-8"}, previewer.previewReq.PreferredModels)
	assert.False(t, previewer.previewReq.TrainingAllowed)
	assert.Empty(t, anthropicProvider.proxyBodies)
	assert.Empty(t, openAIProvider.proxyBodies)
}

func TestService_AgentShadowEvaluationForcesEphemerallyWithoutServingRouter(t *testing.T) {
	telemetry := newCaptureTelemetry()
	pins := newFakePinStore()
	pins.hasPin = true
	pins.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}
	provider := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_eval","type":"message","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":3}}`)
	}}
	servingRouter := &fakeRouter{err: errors.New("serving router must not run")}
	svc := proxy.NewService(servingRouter, map[string]providers.Client{
		providers.ProviderAnthropic: provider,
	}, nil, false, nil, pins, false, providers.ProviderAnthropic, "claude-haiku-4-5", telemetry).
		WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}}).
		WithAvailableModels(map[string]struct{}{"claude-opus-4-8": {}}).
		WithCompaction(nil, 0.0001)

	ctx := context.WithValue(context.Background(), proxy.AgentShadowEvalContextKey{}, proxy.AgentShadowEvaluation{
		Model: "CLAUDE-OPUS-4-8", RolloutID: "pilot-1", StateID: "state-1",
	})
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, "1791da5d-d0db-494c-8574-859a4cb20d97")
	messages := make([]map[string]any, 0, 17)
	for i := range 8 {
		toolID := fmt.Sprintf("tool_%d", i)
		assistantContent := []map[string]any{
			{"type": "tool_use", "id": toolID, "name": "Read", "input": map[string]any{"file_path": "README.md"}},
		}
		if i == 0 {
			assistantContent = append([]map[string]any{
				{"type": "thinking", "thinking": "historical thought", "signature": "stale-signature"},
			}, assistantContent...)
		}
		messages = append(messages,
			map[string]any{"role": "assistant", "content": assistantContent},
			map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": toolID, "content": fmt.Sprintf("old-result-%d", i)}}},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "make the next edit"})
	body, err := json.Marshal(map[string]any{
		"model": "claude-haiku-4-5", "messages": messages, "max_tokens": 512,
		"thinking": map[string]any{"type": "adaptive"},
	})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	require.NoError(t, svc.ProxyMessages(ctx, body, rec, req))
	assert.Zero(t, servingRouter.routeCalls)
	require.Len(t, provider.proxyBodies, 1)
	assert.Contains(t, string(provider.proxyBodies[0]), `"model":"claude-opus-4-8"`)
	assert.Contains(t, string(provider.proxyBodies[0]), "old-result-0")
	assert.NotContains(t, string(provider.proxyBodies[0]), "stale-signature")
	assert.Equal(t, "claude-opus-4-8", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, providers.ProviderAnthropic, rec.Header().Get(proxy.HeaderRouterProvider))
	assert.Equal(t, proxy.ReasonAgentShadowEval, rec.Header().Get(proxy.HeaderRouterDecision))
	assert.Never(t, func() bool {
		telemetry.mu.Lock()
		defer telemetry.mu.Unlock()
		return len(telemetry.rows) != 0 || len(telemetry.shadowRows) != 0
	}, 100*time.Millisecond, 5*time.Millisecond, "eval traffic must not create serving or policy telemetry")
	pins.mu.Lock()
	defer pins.mu.Unlock()
	assert.Zero(t, pins.getCalls, "eval traffic must not read production session pins")
	assert.Empty(t, pins.upserts, "eval traffic must not write production session pins")
	assert.Empty(t, pins.usages, "eval traffic must not update production session usage")
	assert.Zero(t, pins.incrementCalls)
	assert.Zero(t, pins.resetCalls)
}

func TestService_AgentShadowEvaluationNeverSubstitutesRequestedBaseline(t *testing.T) {
	openAIProvider := &fakeProvider{proxyErr: &providers.UpstreamStatusError{Status: http.StatusServiceUnavailable}}
	anthropicProvider := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
	}}
	svc := proxy.NewService(&fakeRouter{}, map[string]providers.Client{
		providers.ProviderAnthropic: anthropicProvider,
		providers.ProviderOpenAI:    openAIProvider,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithDeploymentKeyedProviders(map[string]struct{}{
			providers.ProviderAnthropic: {},
			providers.ProviderOpenAI:    {},
		}).
		WithAvailableModels(map[string]struct{}{
			"claude-opus-4-8": {},
			"gpt-5.5":         {},
		})

	ctx := context.WithValue(context.Background(), proxy.AgentShadowEvalContextKey{}, proxy.AgentShadowEvaluation{
		Model: "gpt-5.5", RolloutID: "pilot-1", StateID: "state-1",
	})
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"inspect this repository"}],"max_tokens":512}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_ = svc.ProxyMessages(ctx, body, rec, req)

	assert.NotEmpty(t, openAIProvider.proxyBodies, "the planned candidate must be attempted")
	assert.Empty(t, anthropicProvider.proxyBodies, "eval forcing must never dispatch the request baseline")
	assert.Equal(t, "gpt-5.5", rec.Header().Get(proxy.HeaderRouterModel))
}

func TestService_AgentShadowEvaluationNeverRetriesSubscriptionOnDeploymentKey(t *testing.T) {
	provider := &fakeProvider{proxyErr: &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}}
	svc := proxy.NewService(&fakeRouter{}, map[string]providers.Client{
		providers.ProviderAnthropic: provider,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-opus-4-8", nil).
		WithDeploymentKeyedProviders(map[string]struct{}{providers.ProviderAnthropic: {}}).
		WithAvailableModels(map[string]struct{}{"claude-opus-4-8": {}})

	ctx := context.WithValue(context.Background(), proxy.AgentShadowEvalContextKey{}, proxy.AgentShadowEvaluation{
		Model: "claude-opus-4-8", RolloutID: "pilot-1", StateID: "state-1",
	})
	ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, "1791da5d-d0db-494c-8574-859a4cb20d97")
	ctx = context.WithValue(ctx, proxy.AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")
	body := []byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"inspect this repository"}],"max_tokens":512}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	_ = svc.ProxyMessages(ctx, body, rec, req)

	require.Len(t, provider.proxyBodies, 1, "eval traffic must not retry on the paid deployment key")
	require.NotNil(t, provider.proxyCreds[0])
	assert.True(t, provider.proxyCreds[0].OAuth, "the single attempt must use the supplied subscription")
}

func TestService_ProxyOpenAIResponses_CustomToolUsesNativeOpenAIFamily(t *testing.T) {
	provider := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","output":[]}`)
	}}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.5", Reason: "test"}}
	svc := proxy.NewService(fr, map[string]providers.Client{
		providers.ProviderAnthropic: &fakeProvider{},
		providers.ProviderOpenAI:    provider,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	body := []byte(`{"model":"gpt-5.5","input":"apply a patch","reasoning":{"effort":"high"},"tools":[{"type":"custom","name":"apply_patch"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIResponses(context.Background(), body, rec, req))

	require.NotNil(t, fr.capturedReq)
	originalEnvelope, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	assert.Equal(t, originalEnvelope.ReasoningConfigurationSHA256(), fr.capturedReq.ReasoningConfigurationSHA256)
	assert.Equal(t, originalEnvelope.ToolConfigurationSHA256(), fr.capturedReq.ToolConfigurationSHA256)
	assert.Equal(t, map[string]struct{}{providers.ProviderOpenAI: {}}, fr.capturedReq.EnabledProviders)
	require.Len(t, provider.proxyBodies, 1)
	assert.JSONEq(t, `{"model":"gpt-5.5","input":"apply a patch","reasoning":{"effort":"high"},"tools":[{"type":"custom","name":"apply_patch"}]}`, string(provider.proxyBodies[0]))
	assert.Equal(t, providers.EndpointResponses, provider.proxyEndpoints[0])
	assert.JSONEq(t, `{"id":"resp_1","object":"response","output":[]}`, rec.Body.String())
}

func TestService_ProxyOpenAIResponses_CodexPassthroughUsesChatForOpenAICompatProvider(t *testing.T) {
	openRouter := &fakeProvider{proxyResponse: func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenRouter, Model: "deepseek/deepseek-chat", Reason: "test"}}
	svc := proxy.NewService(fr, map[string]providers.Client{
		providers.ProviderOpenAI:     &fakeProvider{},
		providers.ProviderOpenRouter: openRouter,
	}, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil)

	ctx := context.WithValue(context.Background(), proxy.OpenAISubscriptionContextKey{}, "eyJhbGciOiJSUzI1NiJ9.codex.sig")
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, "acct-123")
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, body, rec, req))

	require.Len(t, openRouter.proxyBodies, 1)
	assert.Equal(t, providers.EndpointChatCompletions, openRouter.proxyEndpoints[0])
	assert.Contains(t, string(openRouter.proxyBodies[0]), `"messages"`)
	assert.NotContains(t, string(openRouter.proxyBodies[0]), `"input_text"`)
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

// TestService_ProxyMessages_CrossFormatUpstreamErrorBodyReachesClient guards a
// regression: a cross-format upstream non-2xx (e.g. OpenRouter 402) buffered
// the body inside AnthropicSSETranslator but never flushed it, because
// Finalize was skipped on any non-nil proxyErr. Both the translated body and
// the typed UpstreamStatusError must reach the client/handler.
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

// TestService_ProxyMessages_StripsRoutingMarkerFromInboundHistory guards
// service.go's hygiene fix: the routing-marker text injected on prior
// cross-format responses (✦ **Weave Router** → ...) must not survive into the
// upstream body, or it round-trips and pollutes context on every later turn.
func TestService_ProxyMessages_StripsRoutingMarkerFromInboundHistory(t *testing.T) {
	const markerSentinel = "Weave Router"
	body := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[
			{"role":"user","content":"first prompt"},
			{"role":"assistant","content":[
				{"type":"text","text":"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: top scorer\n\n"},
				{"type":"text","text":"real assistant reply"}
			]},
			{"role":"user","content":[
				{"type":"text","text":"</summary>\n<result>✦ **Weave Router** → claude-haiku-4-5 (anthropic) · reason: tool-result follow-up\n\n</result>"}
			]}
		]
	}`)

	provider := &fakeProvider{}
	svc := makeProxyService(
		router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"},
		map[string]providers.Client{providers.ProviderAnthropic: provider},
	)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, httpReq))

	require.Len(t, provider.proxyBodies, 1)
	upstream := string(provider.proxyBodies[0])
	assert.NotContains(t, upstream, markerSentinel, "routing marker must not reach upstream")
	assert.Contains(t, upstream, "real assistant reply", "non-marker assistant content must survive")
	assert.Contains(t, upstream, "</result>", "wrapper text around an embedded marker must survive")
}

func TestService_ProxyMessages_EmbedOnlyUserMessageFlag(t *testing.T) {
	const firstUserPrompt = "Walk every Go file under router/internal/ and produce a one-paragraph summary of each."
	const secondUserPrompt = "Now narrow it to handlers under internal/api/."
	// embedOnlyUserMessage must keep both user prompts, drop system text,
	// assistant tool_use, and tool_result blocks.
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

// TestService_ProxyMessages_NoPinStoreRunsScorerEveryTurn verifies that
// without a pin store, every turn re-runs the cluster scorer.
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

// OpenRouter speaks OpenAI Chat Completions natively, so an OpenAI-format
// inbound landing on an OpenRouter decision must take the no-translation path.
// Regression: eval harness v0.27 hit "no translation path defined".
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

// Bedrock, Makora, and Together are direct providers served by the
// openaicompat client and must route through the OpenAI-emission case, not
// the default "no translation path" branch. Regression: Makora/Together
// (DeepSeek-V4 primaries) were missing from the old literal dispatch list and
// 502'd in prod. Keying dispatch off the translation family fixes all of
// them.
func TestService_ProxyMessages_DispatchesBedrockMakoraTogether(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
	}{
		{"bedrock", providers.ProviderBedrock, "moonshotai/kimi-k2.5"},
		{"makora", providers.ProviderMakora, "deepseek/deepseek-v4-pro"},
		{"together", providers.ProviderTogether, "deepseek/deepseek-v4-pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProvider{
				proxyResponse: func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
				},
			}
			svc := makeProxyService(
				router.Decision{Provider: tc.provider, Model: tc.model, Reason: "test"},
				map[string]providers.Client{tc.provider: p},
			)
			// Tools + opus keeps this out of the classifier-hard-pin path,
			// so the test exercises the widened switch, not the fallback.
			body := []byte(`{"model":"claude-opus-4-7","max_tokens":16,"tools":[{"name":"calc","description":"add","input_schema":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"]}}],"messages":[{"role":"user","content":"What is 7+5? Use calc."}]}`)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
			require.NoError(t, svc.ProxyMessages(context.Background(), body, rec, req))
			require.Len(t, p.proxyBodies, 1, "%s must reach the upstream", tc.provider)
		})
	}
}

func TestService_ProxyOpenAIChatCompletion_DispatchesBedrockMakoraTogether(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
	}{
		{"bedrock", providers.ProviderBedrock, "qwen/qwen3-coder-next"},
		{"makora", providers.ProviderMakora, "deepseek/deepseek-v4-pro"},
		{"together", providers.ProviderTogether, "deepseek/deepseek-v4-pro"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProvider{
				proxyResponse: func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion"}`)
				},
			}
			svc := makeProxyService(
				router.Decision{Provider: tc.provider, Model: tc.model, Reason: "test"},
				map[string]providers.Client{tc.provider: p},
			)
			body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			require.NoError(t, svc.ProxyOpenAIChatCompletion(context.Background(), []byte(body), rec, req))
			require.Len(t, p.proxyBodies, 1, "%s must reach the upstream", tc.provider)
		})
	}
}

// TestService_CodexPassthrough_RoutesFreelyWithBothSubs guards against a
// Codex (ChatGPT) subscription forcing OpenAI-only routing when a Claude
// subscription is also present. Both should stay eligible so the scorer can
// route freely and the sub matching the chosen model pays. (Single-sub
// callers are unaffected: a lone Codex sub still yields {OpenAI}.)
func TestService_CodexPassthrough_RoutesFreelyWithBothSubs(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6"}}
	// Anthropic upstream returns a normal Messages response; proxy translates
	// it to Responses wire format (not the verbatim Codex-backend path).
	anthropicResp := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	providerMap := map[string]providers.Client{
		providers.ProviderOpenAI:    &fakeProvider{},
		providers.ProviderAnthropic: &fakeProvider{proxyResponse: anthropicResp},
	}
	svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-sonnet-4-6", nil).
		WithByokOnly(true)

	// Both subscriptions presented via the dedicated headers (stashed on ctx by
	// the auth middleware): a Codex sub (token + account-id) and a Claude sub.
	ctx := context.WithValue(context.Background(), proxy.OpenAISubscriptionContextKey{}, "eyJhbGciOiJSUzI1NiJ9.codex.sig")
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, "acct-123")
	ctx = context.WithValue(ctx, proxy.AnthropicSubscriptionContextKey{}, "sk-ant-oat01-subscription-token")

	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIResponses(ctx, body, rec, req))

	require.NotNil(t, fr.capturedReq)
	assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenAI,
		"the Codex subscription must keep OpenAI eligible")
	assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic,
		"Codex passthrough must NOT force OpenAI-only when a Claude subscription is also present")

	// Response must be in Responses shape, not chat-completions, proving this
	// didn't go through the verbatim Codex-backend path.
	out := rec.Body.String()
	assert.Contains(t, out, `"object":"response"`)
	assert.Contains(t, out, `"output_text"`)
	assert.NotContains(t, out, "chat.completion")
}

// TestService_WithByokOnly_FiltersUnauthedProvidersFromScorer: with BYOK-only,
// providers without per-request creds must be excluded, or argmax 402s.
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
		// A client x-api-key on the Anthropic surface is a legitimate passthrough
		// credential and enables Anthropic, but must not leak into OpenRouter or
		// other OpenAI-compat upstreams on a different inbound surface.
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

	t.Run("byok-on Anthropic surface with inbound subscription Bearer enables Anthropic only", func(t *testing.T) {
		// A Claude subscription OAuth bearer is legitimate Anthropic auth and
		// enables Anthropic, but must never enable OpenRouter or other
		// OpenAI-compat upstreams — that cross-provider leak was the 2026-05-13
		// prod incident (argmax picked OpenRouter, 401'd with no OpenRouter key).
		fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}
		svc := proxy.NewService(fr, providerMap, nil, false, nil, nil, false, providers.ProviderAnthropic, "claude-haiku-4-5", nil).
			WithByokOnly(true)

		rec := httptest.NewRecorder()
		httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
		httpReq.Header.Set("Authorization", "Bearer sk-ant-oat01-claude-code-token")
		_ = svc.ProxyMessages(context.Background(), body, rec, httpReq)

		require.NotNil(t, fr.capturedReq)
		assert.Contains(t, fr.capturedReq.EnabledProviders, providers.ProviderAnthropic,
			"a Claude subscription bearer is valid Anthropic auth and enables Anthropic")
		assert.NotContains(t, fr.capturedReq.EnabledProviders, providers.ProviderOpenRouter,
			"inbound Bearer on the Anthropic surface must never leak into OpenRouter (2026-05-13 incident)")
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
