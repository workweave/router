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

	"workweave/router/internal/auth"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/capability"
	"workweave/router/internal/router/cluster"
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
	usages   []sessionpin.Usage
	usageCh  chan struct{}
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
	waitForUpsert(t, store)
}

// In-proc LRU short-circuits the Postgres GET on a hit. The scorer
// still runs every MainLoop turn under the planner, but Tier-2 must
// only be consulted on Tier-1 miss.
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

	// Turn 2: in-proc LRU hit; scorer runs (planner re-eval) but
	// tier-2 must not be consulted.
	rec2 := httptest.NewRecorder()
	httpReq2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec2, httpReq2))
	assert.Equal(t, 2, fr.routeCalls, "planner re-evaluates every MainLoop turn")
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
	assert.Equal(t, "claude-haiku-anthropic-byok", rec.Header().Get("x-router-model"),
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
		nil, false, nil,
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

// TestService_HardPin_BypassesTierCeiling regression-guards the PR #100
// finding: ROUTER_HARD_PIN_MODEL is an explicit operator opt-in that
// MUST win over the requested-model tier ceiling. Before the fix, the
// orchestrator clamped the hard-pin decision and silently rewrote the
// operator's chosen model to the cheapest in-ceiling alternative when
// the operator pinned an over-ceiling (or unknown-tier) model.
func TestService_HardPin_BypassesTierCeiling(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	// Hard-pin set to opus (High); inbound model haiku → ceiling Low.
	// Pre-fix, clampToCeiling would rewrite opus → the resolver's pick.
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
	).WithTierClampResolver(func(_, _ map[string]struct{}, _ capability.Tier) (string, string, bool) {
		// If this fires for a hard-pin decision, the bypass is broken.
		return providers.ProviderAnthropic, "claude-haiku-4-5", true
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// Haiku-requesting body with the compaction system marker → hard-pin
	// triggers; tier ceiling is Low but hard-pin must bypass it.
	body := `{"model":"claude-haiku-4-5","system":"Your task is to create a detailed summary of the conversation so far.","messages":[{"role":"user","content":"go"}]}`
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"), "operator hard-pin must win over the tier ceiling")
}

// haikuClampBody requests a Low-tier model (haiku); the scorer is
// stubbed to return an Opus (High) pick, which violates the ceiling and
// must be clamped down to a Low-tier alternative.
const haikuClampBody = `{
	"model":"claude-haiku-4-5",
	"system":"sys",
	"messages":[{"role":"user","content":"summarize this"}]
}`

// TestService_TierClamp_HaikuRequestedClampsHighScore covers the
// haiku-tier leak that motivated this change: a background haiku call
// whose scorer recommended an Opus/DeepSeek-pro/Gemini-pro pick must be
// clamped to a Low-tier alternative.
func TestService_TierClamp_HaikuRequestedClampsHighScore(t *testing.T) {
	store := newFakePinStore()
	// Scorer returns High-tier model: must be rewritten.
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	svc := newPinSvc(fr, store).WithTierClampResolver(func(_, _ map[string]struct{}, ceiling capability.Tier) (string, string, bool) {
		require.Equal(t, capability.TierLow, ceiling, "haiku requested → Low ceiling")
		return providers.ProviderAnthropic, "claude-haiku-4-5", true
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(haikuClampBody), rec, httpReq))

	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"), "decision must be clamped to in-ceiling model")
}

// TestService_TierClamp_OpusRequestedNoClamp confirms High-tier
// requests (opus) leave any decision unchanged — there's no ceiling
// to enforce above High.
func TestService_TierClamp_OpusRequestedNoClamp(t *testing.T) {
	store := newFakePinStore()
	// Scorer returns a High model; opus ceiling allows it.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	resolverCalls := 0
	svc := newPinSvc(fr, store).WithTierClampResolver(func(_, _ map[string]struct{}, _ capability.Tier) (string, string, bool) {
		resolverCalls++
		return "", "", false
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"), "opus ceiling allows High picks unchanged")
	assert.Equal(t, 0, resolverCalls, "resolver must not be called when decision is at or below ceiling")
}

// TestService_TierClamp_PinAboveCeilingIsClamped covers the original
// leak directly: a session pin from a previous opus turn points at
// deepseek-v4-pro (High); the next turn requests haiku (Low ceiling) —
// the pin's stored model must be clamped on read, not blindly served.
// (Pin keying by tier role prevents this in practice; this test guards
// the defense-in-depth clamp on the pin-hit path in case roles collide.)
func TestService_TierClamp_PinAboveCeilingIsClamped(t *testing.T) {
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

	svc := newPinSvc(fr, store).WithTierClampResolver(func(_, _ map[string]struct{}, _ capability.Tier) (string, string, bool) {
		return providers.ProviderAnthropic, "claude-haiku-4-5", true
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(haikuClampBody), rec, httpReq))

	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"))
}

// TestService_TierClamp_UnknownRequestedModelDisablesClamp regression-
// guards the PR #100 finding: RequestedTier must be derived from
// capability.TierFor(feats.Model) directly, not via
// baselineFor(feats.Model). Substituting unknown model names through
// baselineFor (which falls back to the default mid-tier baseline)
// would force custom/proxy model names like "weave-router" into a
// TierMid ceiling and silently clamp high-tier scorer picks. The
// documented behavior is "unknown requested model ⇒ TierUnknown ⇒
// clamping disabled".
func TestService_TierClamp_UnknownRequestedModelDisablesClamp(t *testing.T) {
	store := newFakePinStore()
	// Scorer returns a High-tier pick that a TierMid clamp would
	// rewrite; we want to prove the clamp is disabled entirely.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	resolverCalls := 0
	svc := newPinSvc(fr, store).WithTierClampResolver(func(_, _ map[string]struct{}, _ capability.Tier) (string, string, bool) {
		resolverCalls++
		return providers.ProviderAnthropic, "claude-sonnet-4-5", true
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	// Custom/proxy model name with no capability entry → TierUnknown.
	body := `{"model":"weave-router","system":"sys","messages":[{"role":"user","content":"hi"}]}`
	require.NoError(t, svc.ProxyMessages(ctx, []byte(body), rec, httpReq))

	assert.Equal(t, 0, resolverCalls, "unknown requested model must yield TierUnknown ceiling, disabling the clamp")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"), "scorer pick must pass through unclamped for unknown requested models")
}

// TestService_TierClamp_ExcludedModelsThreadedToResolver regression-
// guards the PR #100 security finding: tier clamping must respect the
// request's ExcludedModels denylist so the clamp path cannot route to
// a model the installation/request has explicitly blocked. Without
// threading req.ExcludedModels through, the resolver would happily
// pick a denylisted model whenever the scorer's choice violated the
// ceiling — silently bypassing model access policy.
func TestService_TierClamp_ExcludedModelsThreadedToResolver(t *testing.T) {
	store := newFakePinStore()
	// Scorer returns an over-ceiling pick; clamp will fire.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	var capturedExcluded map[string]struct{}
	svc := newPinSvc(fr, store).
		WithExcludedModelsOverride([]string{"claude-haiku-4-5"}).
		WithTierClampResolver(func(_, excluded map[string]struct{}, _ capability.Tier) (string, string, bool) {
			capturedExcluded = excluded
			return providers.ProviderAnthropic, "claude-sonnet-4-5", true
		})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(haikuClampBody), rec, httpReq))

	require.NotNil(t, capturedExcluded, "resolver must receive the request's ExcludedModels")
	_, denied := capturedExcluded["claude-haiku-4-5"]
	assert.True(t, denied, "ExcludedModels must propagate to the tier-clamp resolver")
}

// TestService_TierClamp_StaleFlagClearedOnUnclampedFinal regression-
// guards the case Cursor flagged on PR #100: when the orchestrator
// clamps an early-stage decision (e.g. the fresh scorer output) and a
// later stage picks an in-ceiling pin without needing to clamp, the
// TierClamped/PreClampModel fields must reflect the *final* decision —
// otherwise the structured log falsely reports a clamp that didn't
// actually affect the served model.
func TestService_TierClamp_StaleFlagClearedOnUnclampedFinal(t *testing.T) {
	store := newFakePinStore()
	// Pin is in-ceiling for an opus request (TierHigh) — no clamp at the
	// pin-hit step.
	store.hasPin = true
	store.pin = sessionpin.Pin{
		Provider:      providers.ProviderAnthropic,
		Model:         "claude-opus-4-7",
		Reason:        "cluster:v0.37",
		PinnedUntil:   time.Now().Add(30 * time.Minute),
		FirstPinnedAt: time.Now().Add(-5 * time.Minute),
		// Prior-turn usage so the planner has eviction-cost evidence to
		// weigh against the fresh decision; with this set the planner
		// returns OutcomeStay (cache-warm beats switch).
		LastInputTokens: 50000,
	}
	// Fresh scorer returns a violating model (haiku ceiling would have
	// clamped); irrelevant here because we go through the High path.
	fr := &fakeRouter{decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-opus-4-7", Reason: "cluster:v0.37"}}

	clampCalls := 0
	svc := newPinSvc(fr, store).WithTierClampResolver(func(_, _ map[string]struct{}, _ capability.Tier) (string, string, bool) {
		clampCalls++
		// Resolver should never need to fire because all decisions are
		// at or below the High ceiling.
		return providers.ProviderAnthropic, "claude-haiku-4-5", true
	})

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"))
	assert.Equal(t, 0, clampCalls, "no clamp should fire under a High ceiling with in-ceiling models")
	// Implicit: by passing through without clamp, the log would record
	// tier_clamped=false / pre_clamp_model="" — the regression would
	// have left a stale true here from a prior-stage clamp.
}
