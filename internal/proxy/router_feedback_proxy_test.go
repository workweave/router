package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"

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
