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
		map[string]providers.Client{"anthropic": &fakeProvider{}},
		nil,   // emitter
		false, // embedLastUserMessage
		0,     // stickyDecisionTTL — disable legacy LRU for clarity
		nil,   // decisionLog
		nil,   // semanticCache
		store,
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
	assert.Equal(t, "claude-haiku-4-5", rec.Header().Get("x-router-model"),
		"the response must reflect the pinned model, not the fresh-route model")
	waitForUpsert(t, store)
}

func TestService_SessionPin_InProcCacheAvoidsPostgresOnSecondTurn(t *testing.T) {
	store := newFakePinStore()
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())

	// Turn 1: no pin yet → fresh route + async upsert + LRU populate.
	rec1 := httptest.NewRecorder()
	httpReq1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec1, httpReq1))
	waitForUpsert(t, store)
	require.Equal(t, 1, fr.routeCalls)
	require.Equal(t, 1, store.getCalls, "tier-1 miss must consult tier-2 once")

	// Turn 2: same session — must hit the in-proc LRU and skip Postgres.
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

	assert.Equal(t, 1, fr.routeCalls,
		"a pin-store error must fall through to the cluster scorer (fail-open per D5), not block routing")
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

	assert.Equal(t, 1, fr.routeCalls,
		"a pin past its pinned_until must not be served — sweep races mean expired rows can sit in the table")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get("x-router-model"))
}

func TestService_SessionPin_EvalOverrideHeaderBypassesAllTiers(t *testing.T) {
	store := newFakePinStore()
	store.hasPin = true
	store.pin = sessionpin.Pin{Provider: "anthropic", Model: "claude-haiku-4-5", PinnedUntil: time.Now().Add(time.Hour)}
	fr := &fakeRouter{decision: router.Decision{Provider: "anthropic", Model: "claude-opus-4-7", Reason: "fresh"}}
	svc := newPinSvc(fr, store)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	httpReq.Header.Set("x-weave-cluster-version", "v0.2")
	require.NoError(t, svc.ProxyMessages(ctx, []byte(pinTestBody), rec, httpReq))

	assert.Equal(t, 1, fr.routeCalls,
		"eval-override header must bypass the pin tiers so the harness can compare router strategies on the same prompt")
	assert.Equal(t, 0, store.getCalls,
		"the pin store must not even be consulted when bypass is active")
}
