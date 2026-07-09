package proxy_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

type fakeFeedbackStore struct {
	mu     sync.Mutex
	events []proxy.RouterFeedbackEvent
}

func (f *fakeFeedbackStore) InsertRouterFeedback(ctx context.Context, p proxy.RouterFeedbackEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, p)
	return nil
}

type fakePolicyFeedbackRouter struct {
	mu       sync.Mutex
	decision router.Decision
	requests []router.Request
	payloads []map[string]interface{}
}

func (f *fakePolicyFeedbackRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	return f.decision, nil
}

func (f *fakePolicyFeedbackRouter) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.payloads = append(f.payloads, payload)
	return nil
}

func (f *fakePolicyFeedbackRouter) Payloads() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]interface{}(nil), f.payloads...)
}

func (f *fakePolicyFeedbackRouter) Requests() []router.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]router.Request(nil), f.requests...)
}

type blockingPolicyFeedbackRouter struct {
	started chan bool
	release chan struct{}
}

func (f *blockingPolicyFeedbackRouter) Route(ctx context.Context, req router.Request) (router.Decision, error) {
	return router.Decision{}, nil
}

func (f *blockingPolicyFeedbackRouter) ReportFeedback(ctx context.Context, payload map[string]interface{}) error {
	_, hasDeadline := ctx.Deadline()
	select {
	case f.started <- hasDeadline:
	default:
	}
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestService_RouterFeedbackCommand_PersistsAndAcks(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/router-feedback got stuck on Haiku for too long"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvc(fr, store).WithRouterFeedbackStore(feedback)

	installationID := uuid.New().String()
	ctx := authedCtx(installationID)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "router-feedback command must short-circuit routing")
	require.Len(t, feedback.events, 1)
	ev := feedback.events[0]
	assert.Equal(t, installationID, ev.InstallationID)
	assert.Equal(t, "got stuck on Haiku for too long", ev.Feedback)
	assert.Equal(t, "claude-haiku-4-5", ev.ServedModel, "served_model comes from the session pin's last served model")
	assert.Equal(t, "claude-sonnet-4-6", ev.RequestedModel)
	assert.NotEmpty(t, ev.SessionKey)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "message", resp["type"])
	blocks, ok := resp["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	assert.Contains(t, text, "Feedback recorded")
}

func TestService_RouterFeedbackCommand_ForwardsPolicyFeedback(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf- label=\"Complex Followup\" model=\"anthropic/claude-sonnet-5\" should have used the deeper route"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	policyFeedback := &fakePolicyFeedbackRouter{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvc(fr, store).
		WithRouterFeedbackStore(feedback).
		WithHMMRouter(policyFeedback)

	installationID := uuid.New().String()
	ctx := router.WithStrategy(authedCtx(installationID), router.StrategyHMM)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls)
	require.Len(t, feedback.events, 1)
	require.Eventually(t, func() bool {
		return len(policyFeedback.Payloads()) == 1
	}, time.Second, 10*time.Millisecond)
	payloads := policyFeedback.Payloads()
	require.Len(t, payloads, 1)
	payload := payloads[0]
	assert.Equal(t, "down", payload["rating"])
	assert.Equal(t, "label=\"Complex Followup\" model=\"anthropic/claude-sonnet-5\" should have used the deeper route", payload["feedback"])
	assert.Equal(t, "claude-sonnet-4-6", payload["requested_model"])
	assert.Equal(t, "claude-haiku-4-5", payload["served_model"])
	assert.Equal(t, installationID, payload["installation_id"])
	assert.NotEmpty(t, payload["feedback_key"])
	assert.NotEmpty(t, payload["feedback_role"])
}

func TestService_RouterFeedbackCommand_AcksBeforePolicyFeedbackCompletes(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf+"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	policyFeedback := &blockingPolicyFeedbackRouter{
		started: make(chan bool, 1),
		release: make(chan struct{}),
	}
	defer close(policyFeedback.release)
	svc := newPinSvc(fr, store).WithHMMRouter(policyFeedback)

	ctx := router.WithStrategy(authedCtx(uuid.NewString()), router.StrategyHMM)
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	done := make(chan error, 1)
	go func() {
		done <- svc.ProxyMessages(ctx, []byte(body), rec, httpReq)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("router-feedback acknowledgment waited for policy feedback")
	}
	select {
	case hasDeadline := <-policyFeedback.started:
		assert.True(t, hasDeadline, "background policy feedback must have a bounded context")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for policy feedback dispatch")
	}

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	blocks, ok := resp["content"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	assert.Contains(t, first["text"], "Feedback recorded")
}

func TestService_RouterFeedbackCommand_CorrelatesCompactedHMMRoute(t *testing.T) {
	routeBody := []byte(`{
		"model":"claude-haiku-4-5",
		"max_tokens":195000,
		"messages":[
			{"role":"user","content":"` + strings.Repeat("x", 30_000) + `"},
			{"role":"assistant","content":"working"},
			{"role":"user","content":"latest request"}
		]
	}`)
	routeEnv, err := translate.ParseAnthropic(routeBody)
	require.NoError(t, err)
	rawSessionKey := proxy.DeriveSessionKey(routeEnv, "key-1")
	rawFeedbackKey := hex.EncodeToString(rawSessionKey[:])

	store := newFakePinStore()
	policyFeedback := &fakePolicyFeedbackRouter{decision: router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-haiku-4-5",
		Reason:   "hmm_policy(label=Simple Followup)",
		Metadata: &router.RoutingMetadata{Strategy: string(router.StrategyHMM)},
	}}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	svc := newPinSvc(fr, store).
		WithHMMRouter(policyFeedback).
		WithAvailableModels(map[string]struct{}{"claude-haiku-4-5": {}}).
		WithCompaction(nil, proxy.DefaultCompactionTriggerPct)
	ctx := router.WithStrategy(authedCtx(uuid.NewString()), router.StrategyHMM)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, routeBody, httptest.NewRecorder(), httpReq))

	requests := policyFeedback.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, rawFeedbackKey, requests[0].FeedbackKey)

	feedbackBody := []byte(`{
		"model":"claude-haiku-4-5",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"` + strings.Repeat("x", 30_000) + `"},
			{"role":"assistant","content":"working"},
			{"role":"user","content":"latest request"},
			{"role":"assistant","content":"done"},
			{"role":"user","content":"/rf+"}
		]
	}`)
	require.NoError(t, svc.ProxyMessages(ctx, feedbackBody, httptest.NewRecorder(), httpReq))
	require.Eventually(t, func() bool {
		return len(policyFeedback.Payloads()) == 1
	}, time.Second, 10*time.Millisecond)
	payloads := policyFeedback.Payloads()
	require.Len(t, payloads, 1)
	assert.Equal(t, requests[0].FeedbackKey, payloads[0]["feedback_key"])
	assert.Equal(t, requests[0].FeedbackRole, payloads[0]["feedback_role"])
}

func TestService_RouterFeedbackCommand_DoesNotForwardPolicyFeedbackOutsideHMM(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf+"}
		]
	}`
	store := newFakePinStore()
	policyFeedback := &fakePolicyFeedbackRouter{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvc(fr, store).WithHMMRouter(policyFeedback)

	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(authedCtx(uuid.NewString()), []byte(body), rec, httpReq))

	assert.Empty(t, policyFeedback.Payloads())
}

func TestService_RouterFeedbackCommand_OpenAIIngress(t *testing.T) {
	const body = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"/router-feedback wrong model for this refactor"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIPinSvc(fr, store).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls)
	require.Len(t, feedback.events, 1)
	assert.Equal(t, "wrong model for this refactor", feedback.events[0].Feedback)
	assert.Empty(t, feedback.events[0].ServedModel, "no session pin → empty served_model")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chat.completion", resp["object"])
	choices, _ := resp["choices"].([]any)
	require.NotEmpty(t, choices)
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	content, _ := msg["content"].(string)
	assert.Contains(t, content, "Feedback recorded")
}

func TestService_RouterFeedbackCommand_EmptyFeedbackAsksForText(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/router-feedback"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvc(fr, store).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "bare command must still short-circuit, not reach an upstream")
	assert.Empty(t, feedback.events, "empty feedback must not be persisted")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	blocks, _ := resp["content"].([]any)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	assert.Contains(t, text, "needs a verdict or a note")
}

func TestService_RouterFeedbackCommand_ThumbsUpShortcutPersists(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf+"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvc(fr, store).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "rating shortcut must short-circuit routing")
	require.Len(t, feedback.events, 1, "a verdict-only rating must still persist")
	ev := feedback.events[0]
	assert.Equal(t, "up", ev.Rating)
	assert.Equal(t, "👍", ev.Feedback, "verdict-only submission stores a compact label")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	blocks, _ := resp["content"].([]any)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	assert.Contains(t, text, "👍", "the ack echoes the recorded verdict")
}

func TestService_RouterFeedbackCommand_ThumbsDownShortcutWithNote(t *testing.T) {
	const body = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"user","content":"/rf- wrong model for this refactor"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIPinSvc(fr, store).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, httpReq))

	require.Len(t, feedback.events, 1)
	assert.Equal(t, "down", feedback.events[0].Rating)
	assert.Equal(t, "wrong model for this refactor", feedback.events[0].Feedback, "the note is stored verbatim alongside the verdict")
}
