package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
)

// stubPinStore is the minimum sessionpin.Store needed to exercise the
// recordTurnUsage LRU-coherence path without pulling in the external
// fake from service_session_pin_test.go (different test package).
type stubPinStore struct {
	mu          sync.Mutex
	lastUsage   sessionpin.Usage
	usageCalled chan struct{}
}

func newStubPinStore() *stubPinStore {
	return &stubPinStore{usageCalled: make(chan struct{}, 1)}
}

func (s *stubPinStore) Get(context.Context, [sessionpin.SessionKeyLen]byte, string) (sessionpin.Pin, bool, error) {
	return sessionpin.Pin{}, false, nil
}

func (s *stubPinStore) Upsert(context.Context, sessionpin.Pin) error { return nil }

func (s *stubPinStore) UpdateUsage(_ context.Context, _ [sessionpin.SessionKeyLen]byte, _ string, u sessionpin.Usage) error {
	s.mu.Lock()
	s.lastUsage = u
	s.mu.Unlock()
	select {
	case s.usageCalled <- struct{}{}:
	default:
	}
	return nil
}

func (s *stubPinStore) SweepExpired(context.Context) error { return nil }

// TestRecordTurnUsage_UpdatesInProcCache guards the LRU-coherence
// invariant: when recordTurnUsage persists usage, the in-proc pin cache
// entry for the same session must reflect the new Last* fields. Without
// this, loadPin's Tier-1 hit serves a stale zero-usage pin and the
// planner returns ReasonNoPriorUsage forever (the 30s LRU TTL keeps
// resetting under typical agentic turn cadence), silently disabling
// EV-based switching for all active sessions.
func TestRecordTurnUsage_UpdatesInProcCache(t *testing.T) {
	store := newStubPinStore()
	// NewService wires a real expirable LRU when pinSessionTTL/etc. are
	// in play; constructing through the public ctor keeps that wiring
	// honest. We don't need a router/provider for this test — only the
	// usage path.
	svc := NewService(
		nil,
		nil,
		nil,
		false,
		nil,
		store,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	require.NotNil(t, svc.pinCache, "Service must wire an in-proc pin cache when pinStore is set")

	var sessionKey [sessionpin.SessionKeyLen]byte
	for i := range sessionKey {
		sessionKey[i] = byte(i + 1)
	}
	cacheKey := sessionPinCacheKey(sessionKey, sessionpin.DefaultRole)

	// Pre-warm the cache the same way writeNewPin/refreshPin would: a
	// freshly-routed pin with zero usage stats.
	initial := sessionpin.Pin{
		SessionKey:  sessionKey,
		Role:        sessionpin.DefaultRole,
		Provider:    "anthropic",
		Model:       "claude-opus-4-7",
		Reason:      "fresh",
		TurnCount:   1,
		PinnedUntil: time.Now().Add(time.Hour),
	}
	svc.pinCache.Add(cacheKey, initial)

	res := turnLoopResult{
		Decision:   router.Decision{Provider: "anthropic", Model: "claude-opus-4-7"},
		SessionKey: sessionKey,
	}
	svc.recordTurnUsage(res, 1200, 80, 200, 900)

	select {
	case <-store.usageCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected UpdateUsage on the store within 2s; none observed")
	}

	got, ok := svc.pinCache.Get(cacheKey)
	require.True(t, ok, "LRU entry must survive recordTurnUsage")
	assert.Equal(t, 1200, got.LastInputTokens, "LRU LastInputTokens must reflect recorded usage")
	assert.Equal(t, 900, got.LastCachedReadTokens, "LRU LastCachedReadTokens must reflect recorded usage")
	assert.Equal(t, 200, got.LastCachedWriteTokens, "LRU LastCachedWriteTokens must reflect recorded usage")
	assert.Equal(t, 80, got.LastOutputTokens, "LRU LastOutputTokens must reflect recorded usage")
	assert.False(t, got.LastTurnEndedAt.IsZero(), "LRU LastTurnEndedAt must be stamped — the planner uses IsZero() as its no-prior-usage gate")
}

// TestLoadPin_EvictsExpiredLRUEntry guards the LRU tier's expiry check.
// recordTurnUsage refreshes LRU entries on every turn, so the 30s eviction
// clock keeps resetting under typical agentic cadence. Without checking
// PinnedUntil on the LRU hit path, an entry whose pin has lapsed could
// keep being served well past its intended expiry — particularly when
// refreshPin's bounded enqueue drops on semaphore saturation while
// recordTurnUsage keeps landing.
func TestLoadPin_EvictsExpiredLRUEntry(t *testing.T) {
	store := newStubPinStore()
	svc := NewService(
		nil,
		nil,
		nil,
		false,
		nil,
		store,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	require.NotNil(t, svc.pinCache)

	var sessionKey [sessionpin.SessionKeyLen]byte
	for i := range sessionKey {
		sessionKey[i] = byte(i + 1)
	}
	cacheKey := sessionPinCacheKey(sessionKey, sessionpin.DefaultRole)

	// Pre-warm the LRU with a pin whose PinnedUntil is already in the past.
	// The stub pin store returns no rows on Get, so any non-expired LRU
	// entry would be the only source of a "found" result. Confirming a
	// miss here proves the expiry check fires before the LRU is served.
	expired := sessionpin.Pin{
		SessionKey:  sessionKey,
		Role:        sessionpin.DefaultRole,
		Provider:    "anthropic",
		Model:       "claude-opus-4-7",
		Reason:      "fresh",
		TurnCount:   1,
		PinnedUntil: time.Now().Add(-time.Minute),
	}
	svc.pinCache.Add(cacheKey, expired)

	pin, found := svc.loadPin(context.Background(), cacheKey, sessionKey, sessionpin.DefaultRole)
	assert.False(t, found, "expired LRU entry must not be served")
	assert.Equal(t, sessionpin.Pin{}, pin, "miss must return the zero pin")
	_, stillCached := svc.pinCache.Get(cacheKey)
	assert.False(t, stillCached, "expired LRU entry must be evicted on lookup so subsequent turns hit Postgres")
}

// TestLoadPin_ServesFreshLRUEntry is the companion happy-path test: a
// non-expired LRU entry hits Tier 1 without touching Postgres.
func TestLoadPin_ServesFreshLRUEntry(t *testing.T) {
	store := newStubPinStore()
	svc := NewService(
		nil,
		nil,
		nil,
		false,
		nil,
		store,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)

	var sessionKey [sessionpin.SessionKeyLen]byte
	for i := range sessionKey {
		sessionKey[i] = byte(i + 1)
	}
	cacheKey := sessionPinCacheKey(sessionKey, sessionpin.DefaultRole)

	fresh := sessionpin.Pin{
		SessionKey:  sessionKey,
		Role:        sessionpin.DefaultRole,
		Provider:    "anthropic",
		Model:       "claude-opus-4-7",
		Reason:      "fresh",
		TurnCount:   1,
		PinnedUntil: time.Now().Add(time.Hour),
	}
	svc.pinCache.Add(cacheKey, fresh)

	pin, found := svc.loadPin(context.Background(), cacheKey, sessionKey, sessionpin.DefaultRole)
	require.True(t, found, "non-expired LRU entry should be returned without hitting Postgres")
	assert.Equal(t, fresh.Model, pin.Model)
	assert.Equal(t, fresh.Provider, pin.Provider)
}
