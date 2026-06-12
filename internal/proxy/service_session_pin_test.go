package proxy_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePinStore struct {
	mu               sync.Mutex
	pin              sessionpin.Pin
	hasPin           bool
	getErr           error
	getCalls         int
	upserts          []sessionpin.Pin
	upsertCh         chan struct{}
	usages           []sessionpin.Usage
	usageCh          chan struct{}
	incrementCalls   int
	incrementReturns int // value returned by IncrementUpstreamErrors when hasPin is true
	resetCalls       int
}

func newFakePinStore() *fakePinStore {
	return &fakePinStore{
		upsertCh: make(chan struct{}, 16),
		usageCh:  make(chan struct{}, 16),
	}
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
	f.mu.Lock()
	f.usages = append(f.usages, usage)
	f.mu.Unlock()
	select {
	case f.usageCh <- struct{}{}:
	default:
	}
	return nil
}

func (f *fakePinStore) IncrementUpstreamErrors(ctx context.Context, key [sessionpin.SessionKeyLen]byte, role string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.incrementCalls++
	if !f.hasPin {
		return 0, nil
	}
	return f.incrementReturns, nil
}

func (f *fakePinStore) ResetUpstreamErrors(ctx context.Context, key [sessionpin.SessionKeyLen]byte, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
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

// authedCtxWithExternalKey mirrors authedCtx and additionally stashes one
// BYOK ExternalAPIKey on the context, the way the auth middleware does at
// runtime.
func authedCtxWithExternalKey(installationID, provider string, plaintext []byte) context.Context {
	ctx := authedCtx(installationID)
	keys := []*auth.ExternalAPIKey{{
		InstallationID: installationID,
		Provider:       provider,
		Plaintext:      plaintext,
	}}
	return context.WithValue(ctx, proxy.ExternalAPIKeysContextKey{}, keys)
}

const pinTestBody = `{
	"model":"claude-opus-4-7",
	"system":"sys",
	"messages":[{"role":"user","content":"original prompt"}]
}`

// With a Postgres-tier pin and divergent scorer recommendation, the
// planner stays on the pin (ReasonNoPriorUsage covers the case where no
// turn has completed yet so we have no cache-warm evidence to evict).
// The scorer runs once per turn now (Prism-style re-eval), but the
// pinned model still wins.
func TestService_SessionPin_PostgresHitKeepsPinnedModel(t *testing.T) {
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

	// The scorer runs even on a pin hit (Prism-style re-eval); the
	// planner then keeps the pinned model because we have no prior-turn
	// usage to justify paying eviction cost.
	assert.Equal(t, 1, fr.routeCalls, "scorer runs every MainLoop turn under the planner")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
	waitForUpsert(t, store)
}

// A session pinned to a text-only model must not serve an image-bearing turn.
// The guard drops the pin and routes on the scorer's fresh (image-capable)
// decision instead — without it, the pin would 4xx upstream the moment a
// screenshot lands in the conversation (DeepInfra 405 "does not accept image
// input" on GLM-5.1). The guard must NOT add the pin to ExcludedModels: the
// scorer's own image-input filter drops text-only models softly (with an
// empty-pool fallback), whereas exclusion is a hard filter that would fail
// routing on an OSS-only self-host with no image-capable candidate.
func TestService_SessionPin_ImageTurnEvictsTextOnlyPin(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      "deepinfra",
		Model:         "z-ai/glm-5.1",
		Reason:        "cluster:v0.57",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster:v0.57"}}
	svc := newPinSvc(fr, store)

	imageBody := []byte(`{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what is in this screenshot"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
		]}]
	}`)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, imageBody, rec, httpReq))

	require.NotNil(t, fr.capturedReq, "scorer must run after the text-only pin is dropped")
	assert.True(t, fr.capturedReq.HasImages, "routing request must carry the image signal")
	_, excluded := fr.capturedReq.ExcludedModels["z-ai/glm-5.1"]
	assert.False(t, excluded, "pin must be dropped, not hard-excluded; the scorer's image filter drops it softly")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel),
		"served model must be the image-capable fresh decision, not the text-only pin")
}

// Every turn must consult Postgres for its session pin — there is no
// in-process cache. The scorer still runs every MainLoop turn under the
// planner regardless of pin state.
func TestService_SessionPin_EveryTurnReadsPostgres(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())

	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec1, httpReq1))
	require.Equal(t, 1, fr.routeCalls)
	require.Equal(t, 1, store.getCalls, "first turn must read the pin store")

	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec2, httpReq2))
	assert.Equal(t, 2, fr.routeCalls, "planner re-evaluates every MainLoop turn")
	assert.Equal(t, 2, store.getCalls, "second turn must also read Postgres — there is no in-process cache")
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
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
}

// Eval-override headers must NOT bypass session-key pinning; the
// planner still runs and stays on the pin (no prior usage → cannot
// justify eviction cost).
func TestService_SessionPin_EvalOverrideHeaderKeepsSessionKeyPinning(t *testing.T) {
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

	// Scorer still runs (planner re-eval is unconditional) but the pin
	// wins under ReasonNoPriorUsage.
	assert.Equal(t, 1, fr.routeCalls, "scorer runs every MainLoop turn under the planner")
	assert.Equal(t, 1, store.getCalls)
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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

// In byokOnly mode the per-request hard-pin resolver overrides the
// boot-time hardPin{Provider,Model} so compaction lands on a model the
// request can authenticate to. Resolver receives the request's
// enabled-providers set; here we BYOK only Anthropic and assert the
// hard-pin lands on Anthropic regardless of the boot-time default.
func TestService_HardPin_Compaction_ByokOnly_UsesRequestResolver(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}

	resolver := func(enabled map[string]struct{}) (string, string, bool) {
		if _, ok := enabled[providers.ProviderAnthropic]; ok {
			return providers.ProviderAnthropic, "claude-haiku-anthropic-byok", true
		}
		if _, ok := enabled[providers.ProviderOpenRouter]; ok {
			return providers.ProviderOpenRouter, "deepseek/cheap", true
		}
		return "", "", false
	}

	providerMap := map[string]providers.Client{
		providers.ProviderAnthropic:  &fakeProvider{},
		providers.ProviderOpenRouter: &fakeProvider{},
	}
	// Boot-time hard-pin points at OpenRouter (mimics the buggy managed-mode
	// boot path); the per-request resolver must override it to Anthropic
	// because the installation only BYOKs Anthropic.
	svc := proxy.NewService(
		fr, providerMap, nil, false, nil, store, false,
		providers.ProviderOpenRouter, "deepseek/cheap",
		nil,
	).WithByokOnly(true).WithHardPinResolver(resolver)

	ctx := authedCtxWithExternalKey(uuid.New().String(), providers.ProviderAnthropic, []byte("sk-ant-test"))
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(compactionBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "compaction must bypass the cluster scorer")
	assert.Equal(t, "claude-haiku-anthropic-byok", rec.Header().Get(proxy.HeaderRouterModel),
		"per-request resolver must land hard-pin on the installation's BYOK provider, not the boot-time default")
}

// With no BYOK and no client creds in byokOnly mode the resolver returns
// ok=false; the hard-pin branch must surface ErrClusterUnavailable rather
// than dispatching to the boot-time default that the request can't auth to.
func TestService_HardPin_Compaction_ByokOnly_NoEligibleProviderErrors(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}

	resolver := func(enabled map[string]struct{}) (string, string, bool) {
		if _, ok := enabled[providers.ProviderAnthropic]; ok {
			return providers.ProviderAnthropic, "claude-haiku", true
		}
		return "", "", false
	}

	providerMap := map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}}
	svc := proxy.NewService(
		fr, providerMap, nil, false, nil, store, false,
		providers.ProviderOpenRouter, "deepseek/cheap",
		nil,
	).WithByokOnly(true).WithHardPinResolver(resolver)

	ctx := authedCtx(uuid.New().String()) // no external keys
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	err := svc.ProxyMessages(ctx, []byte(compactionBody), rec, httpReq)
	require.Error(t, err, "hard-pin with no eligible provider must surface an error, not silently dispatch")
	assert.ErrorIs(t, err, cluster.ErrClusterUnavailable,
		"error must be ErrClusterUnavailable so handlers map it to HTTP 503")
}

func TestService_HardPin_CompactionAlwaysRoutesToHaiku(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(compactionBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "compaction must bypass the cluster scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel))

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
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
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
		nil, false, nil,
		store,
		false, providers.ProviderAnthropic, "claude-haiku-4-5",
		nil,
	)
}

// OpenAI ingress: Tier-2 pin hit keeps the pinned model under the
// planner (no prior-turn usage → stay).
func TestService_SessionPin_OpenAI_PostgresHitKeepsPinnedModel(t *testing.T) {
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

	assert.Equal(t, 1, fr.routeCalls, "scorer runs every MainLoop turn under the planner")
	assert.Equal(t, "gpt-5", rec.Header().Get(proxy.HeaderRouterModel))
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

func TestService_SessionPin_OpenAI_ForceModelCommandSetsPin(t *testing.T) {
	const forceBody = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"/force-model gpt-5\nuse this model for now"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(forceBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "force-model command must short-circuit routing")
	require.Len(t, store.upserts, 1)
	assert.Equal(t, "gpt-5", store.upserts[0].Model)
	assert.Equal(t, providers.ProviderOpenAI, store.upserts[0].Provider)
	assert.Equal(t, translate.ReasonUserForceModel, store.upserts[0].Reason)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chat.completion", resp["object"])
	choices, ok := resp["choices"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, choices)
	first, ok := choices[0].(map[string]any)
	require.True(t, ok)
	msg, ok := first["message"].(map[string]any)
	require.True(t, ok)
	content, _ := msg["content"].(string)
	assert.Contains(t, content, "force-model applied: gpt-5")
}

func TestService_SessionPin_OpenAI_UnforceModelCommandClearsPin(t *testing.T) {
	const unforceBody = `{
		"model":"gpt-4o",
		"messages":[
			{"role":"system","content":"You are helpful."},
			{"role":"user","content":"/unforce-model"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(unforceBody), rec, httpReq))

	assert.Equal(t, 0, fr.routeCalls, "unforce-model command must short-circuit routing")
	require.Len(t, store.upserts, 1)
	assert.Equal(t, "user_unforced", store.upserts[0].Reason)
	assert.Empty(t, store.upserts[0].Provider)
	assert.Empty(t, store.upserts[0].Model)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "chat.completion", resp["object"])
	choices, ok := resp["choices"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, choices)
	first, ok := choices[0].(map[string]any)
	require.True(t, ok)
	msg, ok := first["message"].(map[string]any)
	require.True(t, ok)
	content, _ := msg["content"].(string)
	assert.Contains(t, content, "force-model cleared")
}

func TestService_SessionPin_OpenAI_ForceModelCommandStreamShape(t *testing.T) {
	const forceStreamBody = `{
		"model":"gpt-4o",
		"stream":true,
		"messages":[
			{"role":"user","content":"/force-model gpt-5"}
		]
	}`
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-4o", Reason: "cluster"}}
	svc := newOpenAIPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
	require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(forceStreamBody), rec, httpReq))

	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	body := rec.Body.String()
	assert.Contains(t, body, `"object":"chat.completion.chunk"`)
	assert.Contains(t, body, "data: [DONE]")
	assert.NotContains(t, body, `"type":"message"`, "must not emit Anthropic wire format on OpenAI ingress")
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
	assert.Equal(t, "gpt-5", rec.Header().Get(proxy.HeaderRouterModel))
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
		nil, false, nil,
		store,
		hardPinExplore,
		providers.ProviderOpenAI,
		"gpt-4o-mini",
		nil,
	)
}

// TestService_OpenAI_CompactionPhraseDoesNotHardPin regression-guards the
// Codex false-positive: Claude Code's compaction system phrase is a
// Claude-Code-only marker, but pre-fix the detector matched it on any wire
// format. Codex sends an `instructions` field via /v1/responses that gets
// flattened into an OpenAI-style system message, so any incidental mention
// of "compact" / "conversation" used to hard-pin every Codex turn to the
// cheap model — and the resulting session pin made subsequent turns stick.
// Compaction detection is now gated on Anthropic format only.
func TestService_OpenAI_CompactionPhraseDoesNotHardPin(t *testing.T) {
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

	assert.Equal(t, 1, fr.routeCalls, "OpenAI body must run the scorer, not hard-pin")
	assert.Equal(t, "gpt-4o", rec.Header().Get(proxy.HeaderRouterModel))
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
	assert.Equal(t, "gpt-4o-mini", rec.Header().Get(proxy.HeaderRouterModel))
}

// TestService_HardPin_BypassesTierCeiling regression-guards the PR #100
// finding: ROUTER_HARD_PIN_MODEL is an explicit operator opt-in that
// MUST win over the requested-model tier ceiling. Before the fix, the
// orchestrator clamped the hard-pin decision and silently rewrote the
// operator's chosen model to the cheapest in-ceiling alternative when
// the operator pinned an over-ceiling (or unknown-tier) model.
func TestService_HardPin_BypassesTierCeiling(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	// Hard-pin set to opus (High); inbound model haiku. The hard pin is served
	// regardless of the requested tier.
	svc := proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderAnthropic: &fakeProvider{}},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderAnthropic,
		"claude-opus-4-7",
		nil,
	)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// Haiku-requesting body with the compaction system marker → hard-pin
	// triggers; tier ceiling is Low but hard-pin must bypass it.
	body := `{"model":"claude-haiku-4-5","system":"Your task is to create a detailed summary of the conversation so far.","messages":[{"role":"user","content":"go"}]}`
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel), "operator hard-pin must win over the tier ceiling")
}

// haikuClampBody requests a Low-tier model (haiku); the scorer is stubbed to
// return an Opus (High) pick. The router now honors that upgrade rather than
// downgrading it to a cheap in-tier model.
const haikuClampBody = `{
	"model":"claude-haiku-4-5",
	"system":"sys",
	"messages":[{"role":"user","content":"summarize this"}]
}`

// TestService_TierClamp_HaikuRequestedHonorsHighScore covers the policy that
// replaced the haiku-tier downgrade: a haiku-requested turn whose scorer
// recommended an Opus (High) pick is served on Opus. The requested tier is a
// floor for the router's judgement, not a ceiling — previously this clamped
// down to the fastest in-ceiling model (gemini-flash-lite).
func TestService_TierClamp_HaikuRequestedHonorsHighScore(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(haikuClampBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel), "haiku-requested hard turn must honor the scorer's Opus upgrade, not clamp down")
}

// TestService_TierClamp_OpusRequestedDecisionPassesThrough confirms an
// opus-requested turn serves the scorer's High pick unchanged.
func TestService_TierClamp_OpusRequestedDecisionPassesThrough(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel), "scorer's pick must pass through unchanged")
}

// TestService_TierClamp_PinAboveRequestedTierHonored covers the pin-hit path:
// a session pin from a previous opus turn points at opus (High); the next turn
// requests haiku (Low). The pin's stronger model is served unchanged — the
// requested tier no longer downgrades it.
func TestService_TierClamp_PinAboveRequestedTierHonored(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      providers.ProviderAnthropic,
		Model:         "claude-opus-4-7",
		Reason:        "cluster:v0.37",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(haikuClampBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel), "pin above the requested tier must be honored, not clamped down")
}

// TestService_ForcedPin_ReasonStaysUserForced verifies a user-forced pin is
// served unchanged with the plain "user_forced" reason (it is never downgraded).
//
// (formerly two tests, clamped + unclamped; the tier clamp no longer downgrades.)
func TestService_ForcedPin_ReasonStaysUserForced(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-opus-4-7",
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: time.Now().Add(30 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, translate.ReasonUserForceModel, rec.Header().Get(proxy.HeaderRouterDecision), "forced pin keeps the plain user_forced reason")
}

// TestService_LoopEscalationPin_HonoredAsImmutableSticky verifies a
// loop_escalation pin is treated like a /force-model pin: the scorer's (cheap)
// decision is bypassed and the session stays on the escalated opus model.
func TestService_LoopEscalationPin_HonoredAsImmutableSticky(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-opus-4-8",
		Reason:      translate.ReasonLoopEscalation,
		PinnedUntil: time.Now().Add(30 * time.Minute),
	}
	// Scorer would pick a cheap model; the escalation pin must win.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.65"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-8", rec.Header().Get(proxy.HeaderRouterModel), "escalation pin must keep the session on opus")
	assert.Equal(t, translate.ReasonLoopEscalation, rec.Header().Get(proxy.HeaderRouterDecision), "escalation pin must report loop_escalation, not the scorer reason")
}

// buildCyclicLoopBody returns an Anthropic request whose assistant history is a
// wide re-read cycle (same few files Read many times, no edits) — enough to trip
// detectCyclicToolCallLoop.
func buildCyclicLoopBody(t *testing.T, nFiles, total int) []byte {
	t.Helper()
	msgs := []any{map[string]any{"role": "user", "content": "do the task"}}
	for i := 0; i < total; i++ {
		id := "toolu_" + strconv.Itoa(i)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "Read",
					"input": map[string]any{"file_path": "/app/f" + strconv.Itoa(i%nFiles) + ".go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "x"},
			}},
		)
	}
	b, err := json.Marshal(map[string]any{"model": "claude-opus-4-8", "max_tokens": 256, "messages": msgs})
	require.NoError(t, err)
	return b
}

// TestService_LoopEscalation_DoesNotOverwriteUserForcedPin verifies a user's
// explicit /force-model choice outranks auto-escalation: a cyclic loop on a
// forced session is recorded but must NOT replace the user_forced pin with opus.
func TestService_LoopEscalation_DoesNotOverwriteUserForcedPin(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderAnthropic,
		Model:       "claude-haiku-4-5",
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: time.Now().Add(30 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster:v0.65"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, buildCyclicLoopBody(t, 5, 26), rec, httpReq))

	for _, p := range store.upserts {
		assert.NotEqual(t, translate.ReasonLoopEscalation, p.Reason, "escalation must not overwrite a user_forced pin")
	}
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "session stays on the user's forced model, not opus")
}

// TestService_UserForcedPin_IneligibleProviderFallsThrough covers the BYOK
// provider-eligibility check: a forced pin whose provider is not in the
// per-request EnabledProviders set must NOT be served. Otherwise the router
// dispatches a turn to a provider the request has no credentials for,
// resulting in a 401 / silent unauthenticated upstream call.
func TestService_UserForcedPin_IneligibleProviderFallsThrough(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:    providers.ProviderOpenAI, // forced provider NOT in EnabledProviders
		Model:       "gpt-5",
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: time.Now().Add(30 * time.Minute),
	}
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "cluster"}}
	// newPinSvc only registers Anthropic, so EnabledProviders == {anthropic}.
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls, "ineligible-provider forced pin must fall through to the scorer")
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get(proxy.HeaderRouterModel), "must dispatch to the eligible provider, not gpt-5/openai")
}

// The x-weave-force-model header is the headless equivalent of /force-model:
// it must write an immutable user_forced pin for the alias-resolved canonical
// model so the turn loop's user-forced branch serves it for the session.
func TestService_ForceModelHeader_WritesUserForcedPin(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set(proxy.ForceModelHeader, "opus")
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	store.mu.Lock()
	defer store.mu.Unlock()
	var forced *sessionpin.Pin
	for i := range store.upserts {
		if store.upserts[i].Reason == translate.ReasonUserForceModel {
			forced = &store.upserts[i]
			break
		}
	}
	require.NotNil(t, forced, "header must write a user_forced pin upsert")
	assert.Equal(t, "claude-opus-4-8", forced.Model, "alias 'opus' resolves to the canonical id")
	assert.Equal(t, providers.ProviderAnthropic, forced.Provider)
}

// An unrecognized x-weave-force-model value must be ignored: routing proceeds
// automatically and no user_forced pin is written, so a typo can't strand a
// session on an unservable directive.
func TestService_ForceModelHeader_UnknownModelIgnored(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set(proxy.ForceModelHeader, "totally-not-a-model")
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	waitForUpsert(t, store) // normal routing still writes a fresh pin
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.upserts {
		assert.NotEqual(t, translate.ReasonUserForceModel, p.Reason, "unrecognized header must not write a user_forced pin")
	}
}
