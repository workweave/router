package proxy_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
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
			{"role":"user","content":"/rf- label=\"high\" model=\"anthropic/claude-sonnet-5\" should have used the deeper route"}
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
		WithPolicyStrategy(policy.StrategySpec{Strategy: router.StrategyRL, Router: policyFeedback})

	installationID := uuid.New().String()
	ctx := router.WithStrategy(authedCtx(installationID), router.StrategyRL)
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
	assert.Equal(t, "label=\"high\" model=\"anthropic/claude-sonnet-5\" should have used the deeper route", payload["feedback"])
	assert.Equal(t, "claude-sonnet-4-6", payload["requested_model"])
	assert.Equal(t, "claude-haiku-4-5", payload["served_model"])
	assert.Equal(t, installationID, payload["installation_id"])
	assert.Equal(t, string(router.StrategyRL), payload["strategy"])
	assert.NotContains(t, payload, "training_conversation_delta")
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

func TestService_RouterFeedbackCommand_OmitsTrainingTranscriptWithoutPermission(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"first request"},
			{"role":"assistant","content":"first response"},
			{"role":"user","content":"/rf+"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	policyFeedback := &fakePolicyFeedbackRouter{}
	svc := newPinSvc(fr, store).WithHMMRouter(policyFeedback)

	ctx := router.WithStrategy(authedCtx(uuid.NewString()), router.StrategyHMM)
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))))
	require.Eventually(t, func() bool {
		return len(policyFeedback.Payloads()) == 1
	}, time.Second, 10*time.Millisecond)
	payload := policyFeedback.Payloads()[0]
	assert.Equal(t, false, payload["training_allowed"])
	assert.NotContains(t, payload, "training_conversation_delta")
}

func TestService_RouterFeedbackCommand_CorrelatesCompactedHMMEmbeddingRoute(t *testing.T) {
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
		Reason:   "hmm_policy(label=balanced)",
		Metadata: &router.RoutingMetadata{Strategy: string(router.StrategyHMMEmbedding)},
	}}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	svc := newPinSvc(fr, store).
		WithPolicyStrategy(policy.StrategySpec{
			Strategy:    router.StrategyHMMEmbedding,
			Router:      policyFeedback,
			Unavailable: router.ErrStrategyUnavailable,
		}).
		WithAvailableModels(map[string]struct{}{"claude-haiku-4-5": {}}).
		WithCompaction(nil, proxy.DefaultCompactionTriggerPct)
	installationID := uuid.NewString()
	ctx := router.WithStrategy(authedCtx(installationID), router.StrategyHMMEmbedding)
	ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, "org-test")
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, routeBody, httptest.NewRecorder(), httpReq))

	requests := policyFeedback.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, rawFeedbackKey, requests[0].FeedbackKey)
	assert.Equal(t, "org-test", requests[0].OrganizationID)
	assert.Equal(t, installationID, requests[0].InstallationID)

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
	assert.Equal(t, "org-test", payloads[0]["organization_id"])
	assert.Equal(t, true, payloads[0]["training_allowed"])
	delta, ok := payloads[0]["training_conversation_delta"].([]router.ConversationMessage)
	require.True(t, ok)
	require.Len(t, delta, 2)
	assert.Equal(t, "user", delta[0].Role)
	assert.Equal(t, "latest request", delta[0].Text)
	assert.Equal(t, "assistant", delta[1].Role)
	assert.Equal(t, "done", delta[1].Text)
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

type recordingFeedbackRepo struct {
	mu      sync.Mutex
	upserts []proxy.UpsertFeedbackParams
}

func (f *recordingFeedbackRepo) Upsert(_ context.Context, p proxy.UpsertFeedbackParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, p)
	return nil
}

func (f *recordingFeedbackRepo) GetContext(_ context.Context, _, _ string) (proxy.FeedbackContext, error) {
	return proxy.FeedbackContext{}, nil
}

func TestService_RouterFeedbackCommand_SequenceResolvesTelemetryTurn(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 - wrong tier for this"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{RequestID: "req-abc", DecisionModel: "claude-opus-4-7", RouteID: "hmm:xyz"}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	require.Equal(t, []int{-2}, telem.seqCalls, "the parsed relative sequence must be resolved against telemetry")
	require.Len(t, feedback.events, 1)
	ev := feedback.events[0]
	assert.Equal(t, "claude-opus-4-7", ev.ServedModel, "served_model comes from the resolved telemetry row, not the pin")
	assert.Equal(t, "req-abc", ev.RequestID)
	assert.Equal(t, "hmm:xyz", ev.RouteID)
	assert.Equal(t, "down", ev.Rating)
	assert.Equal(t, "wrong tier for this", ev.Feedback)
}

func TestService_RouterFeedbackCommand_SequenceNotFoundAcksGuidance(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -9 too slow"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqErr = sql.ErrNoRows
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "an unresolvable sequence must short-circuit routing")
	assert.Empty(t, feedback.events, "no feedback row is written when the sequence cannot be resolved")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	blocks, _ := resp["content"].([]any)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	assert.Contains(t, text, "No turn found at that sequence number")
}

func TestService_RouterFeedbackCommand_DBErrorFallsBackToPin(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 + wrong tier"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqErr = errors.New("connection refused")
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	require.Len(t, feedback.events, 1, "feedback must persist on transient DB errors, falling back to pin servedModel")
	ev := feedback.events[0]
	assert.Equal(t, "claude-haiku-4-5", ev.ServedModel, "falls back to the pin on transient DB failure")
	assert.Empty(t, ev.RequestID, "no telemetry row, so requestID is empty")
	assert.Equal(t, "up", ev.Rating)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	blocks, _ := resp["content"].([]any)
	require.NotEmpty(t, blocks)
	first, _ := blocks[0].(map[string]any)
	text, _ := first["text"].(string)
	assert.Contains(t, text, "Feedback recorded", "ack shows normally even on transient telemetry error")
}

func TestService_RouterFeedbackCommand_NoSequenceKeepsPinServedModel(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf- too slow"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", LastServedModel: "claude-haiku-4-5"}
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Empty(t, telem.seqCalls, "no sequence means no telemetry lookup")
	require.Len(t, feedback.events, 1)
	ev := feedback.events[0]
	assert.Equal(t, "claude-haiku-4-5", ev.ServedModel, "falls back to the pin's last served model")
	assert.Empty(t, ev.RequestID)
	assert.Empty(t, ev.RouteID)
}

func TestService_RouterFeedbackCommand_SequenceNoteOnlySkipsRequestFeedbackUpsert(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 the diff was incomplete"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{RequestID: "req-note-only", DecisionModel: "claude-opus-4-7"}
	repo := &recordingFeedbackRepo{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback).WithFeedback(repo, nil, "")

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	require.Len(t, feedback.events, 1)
	assert.Empty(t, feedback.events[0].Rating, "note-only submission carries no verdict")
	assert.Empty(t, repo.upserts, "a note-only rating must not upsert into the up/down-only request_feedback table")
}

func TestService_RouterFeedbackCommand_SequenceWithRatingUpsertsRequestFeedback(t *testing.T) {
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 - too slow"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{RequestID: "req-rated", DecisionModel: "claude-opus-4-7"}
	repo := &recordingFeedbackRepo{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).WithRouterFeedbackStore(feedback).WithFeedback(repo, nil, "")

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	require.Len(t, repo.upserts, 1, "a rated, sequence-resolved submission converges onto request_feedback")
	up := repo.upserts[0]
	assert.Equal(t, "req-rated", up.RequestID)
	assert.Equal(t, "down", up.Rating)
	assert.Equal(t, "router-feedback-command", up.Source)
}

func TestService_RouterFeedbackCommand_SequenceResolvesStrategyRoutesToItsReporter(t *testing.T) {
	// Current request context is on the cluster strategy (no policy reporter).
	// The user's `/rf -2` resolves to telemetry whose strategy is RL, which
	// has a registered feedback reporter. Policy feedback must land on RL.
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 + wrong tier"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{
		RequestID:     "req-resolved-on-RL",
		DecisionModel: "claude-opus-4-7",
		Strategy:      "rl",
	}
	hmmReporter := &fakePolicyFeedbackRouter{}
	rlReporter := &fakePolicyFeedbackRouter{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	ctx := authedCtx(uuid.New().String())
	svc := newPinSvcWithTelemetry(fr, store, telem).
		WithRouterFeedbackStore(feedback).
		WithPolicyStrategy(policy.StrategySpec{Strategy: router.StrategyRL, Router: rlReporter}).
		WithHMMRouter(hmmReporter)
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))))

	require.Eventually(t, func() bool { return len(rlReporter.Payloads()) == 1 }, time.Second, 10*time.Millisecond)
	require.Empty(t, hmmReporter.Payloads(), "current request is on cluster; HMM reporter must not be selected just because the current context might happen to be HMM somewhere else")
	payload := rlReporter.Payloads()[0]
	assert.Equal(t, "rl", payload["strategy"], "the resolved turn's strategy must drive both the payload and the reporter")
	assert.Equal(t, "req-resolved-on-RL", payload["request_id"])
	assert.Equal(t, "claude-opus-4-7", payload["served_model"])
	assert.NotContains(t, payload, "training_conversation_delta", "training delta is suppressed for sequence-rated feedback (latest-turn slice is wrong for older turns)")
}

func TestService_RouterFeedbackCommand_SequenceRejectsHMMDeltaWithResolvedStrategy(t *testing.T) {
	// When an HMM-rated historical turn is being rated, the sidecar should
	// receive the rating + resolved-turn identifiers but no mis-paired delta.
	const body = `{
		"model":"claude-sonnet-4-6",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"/rf -2 - too slow"}
		]
	}`
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{
		RequestID:     "req-resolved-hmm",
		DecisionModel: "claude-opus-4-7",
		Strategy:      "hmm_embedding",
	}
	hmmReporter := &fakePolicyFeedbackRouter{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-sonnet-4-6", Reason: "cluster"}}
	ctx := router.WithStrategy(authedCtx(uuid.New().String()), router.StrategyHMMEmbedding)
	svc := newPinSvcWithTelemetry(fr, store, telem).
		WithRouterFeedbackStore(feedback).
		WithPolicyStrategy(policy.StrategySpec{
			Strategy:    router.StrategyHMMEmbedding,
			Router:      hmmReporter,
			Unavailable: router.ErrStrategyUnavailable,
		}).
		WithAvailableModels(map[string]struct{}{"claude-opus-4-7": {}}).
		WithCompaction(nil, proxy.DefaultCompactionTriggerPct)
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))))

	require.Eventually(t, func() bool { return len(hmmReporter.Payloads()) == 1 }, time.Second, 10*time.Millisecond)
	payload := hmmReporter.Payloads()[0]
	assert.Equal(t, "hmm_embedding", payload["strategy"])
	assert.Equal(t, "req-resolved-hmm", payload["request_id"])
	assert.NotContains(t, payload, "training_conversation_delta", "training delta would pair the resolved turn with the wrong conversation")
}

func TestService_RouterFeedbackCommand_NegativeOnePreservesTrainingDelta(t *testing.T) {
	// `/rf -1` is "rate the previous turn" — the latest assistant message
	// in env IS the rated turn, so the training-delta slice matches.
	body := []byte(`{
		"model":"claude-haiku-4-5",
		"max_tokens":1024,
		"messages":[
			{"role":"user","content":"first request"},
			{"role":"assistant","content":"first response"},
			{"role":"user","content":"/rf -1 +"}
		]
	}`)
	store := newFakePinStore()
	feedback := &fakeFeedbackStore{}
	telem := newCaptureTelemetry()
	telem.seqResult = proxy.TelemetryTurnResult{RequestID: "req-prev", DecisionModel: "claude-haiku-4-5", Strategy: "hmm_embedding"}
	hmmReporter := &fakePolicyFeedbackRouter{}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	svc := newPinSvcWithTelemetry(fr, store, telem).
		WithRouterFeedbackStore(feedback).
		WithPolicyStrategy(policy.StrategySpec{
			Strategy:    router.StrategyHMMEmbedding,
			Router:      hmmReporter,
			Unavailable: router.ErrStrategyUnavailable,
		}).
		WithAvailableModels(map[string]struct{}{"claude-haiku-4-5": {}}).
		WithCompaction(nil, proxy.DefaultCompactionTriggerPct)
	ctx := router.WithStrategy(authedCtx(uuid.NewString()), router.StrategyHMMEmbedding)
	ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
	require.NoError(t, svc.ProxyMessages(ctx, body, httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))))
	require.Eventually(t, func() bool { return len(hmmReporter.Payloads()) == 1 }, time.Second, 10*time.Millisecond)
	payload := hmmReporter.Payloads()[0]
	assert.Equal(t, "hmm_embedding", payload["strategy"])
	assert.Equal(t, "req-prev", payload["request_id"])
	delta, ok := payload["training_conversation_delta"].([]router.ConversationMessage)
	require.True(t, ok, "the rated turn is the latest assistant segment in env, so the delta must be present")
	require.Len(t, delta, 2)
	assert.Equal(t, "user", delta[0].Role)
	assert.Equal(t, "first request", delta[0].Text)
	assert.Equal(t, "assistant", delta[1].Role)
	assert.Equal(t, "first response", delta[1].Text)
}
