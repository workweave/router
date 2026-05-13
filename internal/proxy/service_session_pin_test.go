package proxy_test

import (
	"context"
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
	"workweave/router/internal/router/sessionpin"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePinStore struct {
	mu       sync.Mutex
	pin      sessionpin.Pin
	hasPin   bool
	getErr   error
	getCalls int
	upserts  []sessionpin.Pin
	upsertCh chan struct{}
}

func newFakePinStore() *fakePinStore {
	return &fakePinStore{upsertCh: make(chan struct{}, 16)}
}

func (f *fakePinStore) Get(ctx context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	if f.getErr != nil {
		return sessionpin.Pin{}, false, f.getErr
	}
	if !f.hasPin {
		return sessionpin.Pin{}, false, nil
	}
	pin := f.pin
	pin.SessionKey = key
	pin.Role = role
	return pin, true, nil
}

func (f *fakePinStore) Upsert(ctx context.Context, p sessionpin.Pin) error {
	f.mu.Lock()
	f.upserts = append(f.upserts, p)
	f.mu.Unlock()
	select {
	case f.upsertCh <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakePinStore) UpdateUsage(ctx context.Context, key [sessionpin.SessionKeyLen]byte, role string, usage sessionpin.Usage) error {
	return nil
}

func (f *fakePinStore) SweepExpired(ctx context.Context) error { return nil }

func waitForUpsert(t *testing.T, store *fakePinStore) {
	t.Helper()
	select {
	case <-store.upsertCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected an async pin upsert within 2s; none observed")
	}
}

func newPinSvc(fr *fakeRouter, store *fakePinStore) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		0, // stickyDecisionTTL=0 disables legacy LRU
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	)
}

func authedCtx(installationID string) context.Context {
	ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-1")
	return context.WithValue(ctx, proxy.InstallationIDContextKey{}, installationID)
}

const pinTestBody = `{
	"model":"claude-opus-4-7",
	"system":"sys",
	"messages":[{"role":"user","content":"original prompt"}]
}`

func TestService_SessionPin_PostgresHitShortCircuitsRoute(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      "anthropic",
		Model:         "claude-haiku-4-5",
		Reason:        "cluster:v0.2",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster:v0.2"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "tier-2 hit must short-circuit s.router.Route")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
	waitForUpsert(t, store)
}

func TestService_SessionPin_InProcCacheAvoidsPostgresOnSecondTurn(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())

	// Turn 1: fresh route + async upsert + LRU populate.
	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec1, httpReq1))
	waitForUpsert(t, store)
	require.Equal(t, 1, fr.routeCalls)
	require.Equal(t, 1, store.getCalls, "tier-1 miss must consult tier-2 once")

	// Turn 2: in-proc LRU hit; tier-2 must not be consulted.
	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec2, httpReq2))
	assert.Equal(t, 1, fr.routeCalls, "second turn must not re-route")
	assert.Equal(t, 1, store.getCalls, "second turn must be served by tier-1; tier-2 must not be consulted")
}

func TestService_SessionPin_StoreErrorFallsThroughToFreshRoute(t *testing.T) {
	store := newFakePinStore()
	store.getErr = errors.New("postgres unreachable")
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "pin-store error must fall through to the cluster scorer (fail-open per D5)")
}

func TestService_SessionPin_ExpiredPinIsIgnored(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    "anthropic",
		Model:       "claude-haiku-4-5",
		PinnedUntil: time.Now().Add(-1 * time.Minute), // expired
	}
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "expired pin must not be served (sweep races leave stale rows)")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"))
}

func TestService_SessionPin_EvalOverrideHeaderKeepsSessionKeyPinning(t *testing.T) {
	// Tier-1/2 pinning must stay active under eval traffic to keep multi-turn
	// tool-use on a single provider (Gemini rejects function calls without
	// thoughtSignature emitted by other providers).
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: "anthropic", Model: "claude-haiku-4-5", PinnedUntil: time.Now().Add(time.Hour), Reason: "pinned"}
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-weave-cluster-version", "v0.2")
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "eval-override must not bypass session-key pinning")
	assert.Equal(t, 1, store.getCalls)
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
}

// compactionBody triggers the compaction detector (§3.4).
const compactionBody = `{
	"model":"claude-opus-4-7",
	"system":"Your task is to create a detailed summary of the conversation so far.",
	"messages":[{"role":"user","content":"go"}]
}`

// exploreBody marks an Explore sub-agent dispatch (§3.4).
const exploreBody = `{
	"model":"claude-opus-4-7",
	"metadata":{"user_id":"subagent:Explore"},
	"messages":[{"role":"user","content":"list go files"}]
}`

func TestService_HardPin_CompactionAlwaysRoutesToHaiku(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(compactionBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "compaction must bypass the cluster scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
	assert.Equal(t, 0, store.getCalls, "compaction must not consult the pin store")

	select {
	case <-store.upsertCh:
		t.Fatal("compaction turn must not write a session pin (would overwrite main-loop model)")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestService_HardPin_ExploreRoutesToHaikuWhenFlagOn(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		0,
		nil,
		store,
		true,
		providers.ProviderAnthropic,
		"claude-haiku-4-5",
		nil,
	)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(exploreBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "Explore must bypass cluster scorer when hardPinExplore=on")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))

	select {
	case <-store.upsertCh:
		t.Fatal("Explore hard-pin turn must not write a session pin")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestService_HardPin_ExploreFallsThroughWhenFlagOff(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}
	svc := newPinSvc(fr, store) // hardPinExplore=false

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(exploreBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "Explore must fall through when hardPinExplore=off")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"))
}

// OpenAI ingress: same Stage 1 path via ProxyOpenAIChatCompletion.

const openAIPinTestBody = `{
	"model":"gpt-4o",
	"messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":"original prompt"}
	]
}`

func newOpenAIPinSvc(fr *fakeRouter, store *fakePinStore) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderOpenAI:    &fakeProvider{},
		},
		nil, false, 0, nil,
		store,
		false, providers.ProviderAnthropic, "claude-haiku-4-5",
		nil,
	)
}

func TestService_SessionPin_OpenAI_PostgresHitShortCircuitsRoute(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      providers.ProviderOpenAI,
		Model:         "gpt-5",
		Reason:        "cluster:v0.2",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster:v0.2"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(openAIPinTestBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "OpenAI tier-2 hit must short-circuit the cluster scorer")
	assert.Equal(t, "gpt-5", rec.Header().Get("x-router-model"))
	waitForUpsert(t, store)
}

func TestService_SessionPin_OpenAI_FreshRouteCreatesPin(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "fresh"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(openAIPinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "first turn must route fresh")
	waitForUpsert(t, store)
	require.Len(t, store.upserts, 1)
	assert.Equal(t, providers.ProviderOpenAI, store.upserts[0].Provider)
	assert.Equal(t, "gpt-4o", store.upserts[0].Model)
}

func TestService_SessionPin_OpenAI_ToolResultShortCircuit(t *testing.T) {
	// Trailing role=="tool" → turntype.ToolResult. With a pin, short-circuit
	// the scorer (tool-result embeddings are noisy and flip decisions).
	const toolResultBody = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"original prompt"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"t1","content":"ls output"}
		]
	}`
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderOpenAI,
		Model:       "gpt-5",
		Reason:      "cluster:v0.2",
		PinnedUntil: time.Now().Add(30 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "fresh"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(toolResultBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "tool-result with existing pin must not re-run the scorer")
	assert.Equal(t, "gpt-5", rec.Header().Get("x-router-model"))
}

// newOpenAIHardPinSvc configures a service whose hard-pin target is on the
// OpenAI provider, keeping the path same-format (the cross-format Anthropic
// translator needs a real response body to finalize).
func newOpenAIHardPinSvc(fr *fakeRouter, store *fakePinStore, hardPinExplore bool) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderAnthropic: &fakeProvider{},
			providers.ProviderOpenAI:    &fakeProvider{},
		},
		nil, false, 0, nil,
		store,
		hardPinExplore,
		providers.ProviderOpenAI,
		"gpt-4o-mini",
		nil,
	)
}

func TestService_HardPin_OpenAI_CompactionRoutesToHardPin(t *testing.T) {
	const compactionOpenAIBody = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"Your task is to create a detailed summary of the conversation so far."},
			{"role":"user","content":"go"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIHardPinSvc(fr, store, false)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(compactionOpenAIBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "OpenAI compaction must bypass the scorer")
	assert.Equal(t, "gpt-4o-mini", rec.Header().Get("x-router-model"))

	select {
	case <-store.upsertCh:
		t.Fatal("compaction turn must not write a session pin")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestService_HardPin_OpenAI_SubAgentHeaderHintRoutesToHardPin(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIHardPinSvc(fr, store, true)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	httpReq.Header.Set("x-weave-subagent-type", "Explore")
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(openAIPinTestBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "x-weave-subagent-type must trigger Explore hard-pin")
	assert.Equal(t, "gpt-4o-mini", rec.Header().Get("x-router-model"))
}
