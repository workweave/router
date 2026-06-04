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

// stubPinStore is a minimal sessionpin.Store for testing recordTurnUsage.
// getPin/getFound configure the Get response; both default to a miss so
// existing tests that rely on "no Postgres row" keep working unchanged.
type stubPinStore struct {
	mu        sync.Mutex
	lastUsage sessionpin.Usage
	usageHits int
	getPin    sessionpin.Pin
	getFound  bool
}

func newStubPinStore() *stubPinStore {
	return &stubPinStore{}
}

func (s *stubPinStore) Get(context.Context, [sessionpin.SessionKeyLen]byte, string) (sessionpin.Pin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getPin, s.getFound, nil
}

func (s *stubPinStore) Upsert(context.Context, sessionpin.Pin) error { return nil }

func (s *stubPinStore) UpdateUsage(_ context.Context, _ [sessionpin.SessionKeyLen]byte, _ string, u sessionpin.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsage = u
	s.usageHits++
	return nil
}

func (s *stubPinStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	return 0, nil
}

func (s *stubPinStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	return nil
}

func (s *stubPinStore) SweepExpired(context.Context) error { return nil }

// TestRecordTurnUsage_WritesToStore guards the synchronous UpdateUsage write:
// recordTurnUsage must persist Last* fields to Postgres in-line on the request
// path so the planner has prior-turn evidence by the time the next turn loads
// the pin.
func TestRecordTurnUsage_WritesToStore(t *testing.T) {
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

	res := turnLoopResult{
		Decision:   router.Decision{Provider: "anthropic", Model: "claude-opus-4-7"},
		SessionKey: sessionKey,
		PinRole:    sessionpin.DefaultRole,
	}
	svc.recordTurnUsage(res, 1200, 80, 200, 900)

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, 1, store.usageHits, "UpdateUsage must run synchronously on the request path")
	assert.Equal(t, 1200, store.lastUsage.InputTokens)
	assert.Equal(t, 900, store.lastUsage.CachedReadTokens)
	assert.Equal(t, 200, store.lastUsage.CachedWriteTokens)
	assert.Equal(t, 80, store.lastUsage.OutputTokens)
	assert.Equal(t, "claude-opus-4-7", store.lastUsage.ServedModel)
	assert.False(t, store.lastUsage.EndedAt.IsZero(), "EndedAt must be stamped — the planner uses IsZero() as its no-prior-usage gate")
}

// TestLoadPin_DoesNotServeExpiredPostgresPinButKeepsEmitHistory guards the
// expiry filter: expired rows must be routing misses, but their
// has_ever_switched / last_served_model history still protects Anthropic emit
// from poisoned thinking blocks that remain in the client transcript.
func TestLoadPin_DoesNotServeExpiredPostgresPinButKeepsEmitHistory(t *testing.T) {
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
	require.NotNil(t, svc.pinStore)

	var sessionKey [sessionpin.SessionKeyLen]byte
	for i := range sessionKey {
		sessionKey[i] = byte(i + 1)
	}

	store.getPin = sessionpin.Pin{
		SessionKey:      sessionKey,
		Role:            sessionpin.DefaultRole,
		Provider:        "anthropic",
		Model:           "claude-opus-4-7",
		Reason:          "fresh",
		TurnCount:       1,
		PinnedUntil:     time.Now().Add(-time.Minute),
		LastServedModel: "claude-opus-4-7",
		HasEverSwitched: true,
	}
	store.getFound = true

	pin, found := svc.loadPin(context.Background(), sessionKey, sessionpin.DefaultRole)
	assert.False(t, found, "expired Postgres row must not be served")
	assert.Equal(t, "claude-opus-4-7", pin.LastServedModel, "expired row history must be available for emit")
	assert.True(t, pin.HasEverSwitched, "expired row latch must be available for emit")

	res := turnLoopResult{
		Decision:            router.Decision{Model: "claude-opus-4-7"},
		PriorServedModel:    pin.LastServedModel,
		SessionEverSwitched: pin.HasEverSwitched,
	}
	assert.True(t, res.modelSwitched(), "expired switched-session history must still strip thinking blocks")
}

// TestLoadPin_ServesFreshPostgresPin is the companion: a non-expired Postgres
// row is returned verbatim.
func TestLoadPin_ServesFreshPostgresPin(t *testing.T) {
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

	store.getPin = sessionpin.Pin{
		SessionKey:  sessionKey,
		Role:        sessionpin.DefaultRole,
		Provider:    "anthropic",
		Model:       "claude-opus-4-7",
		Reason:      "fresh",
		TurnCount:   1,
		PinnedUntil: time.Now().Add(time.Hour),
	}
	store.getFound = true

	pin, found := svc.loadPin(context.Background(), sessionKey, sessionpin.DefaultRole)
	require.True(t, found, "non-expired Postgres row must be returned")
	assert.Equal(t, "claude-opus-4-7", pin.Model)
	assert.Equal(t, "anthropic", pin.Provider)
}

// TestModelSwitched covers the switch → stay → stay lifecycle that motivated
// the has_ever_switched latch. The transition turn alone is not enough: once a
// session has served two models, the stale-signed thinking blocks persist in
// the client transcript, so every subsequent same-model turn must keep
// stripping or Anthropic 400s.
func TestModelSwitched(t *testing.T) {
	tests := []struct {
		name             string
		priorServedModel string
		decisionModel    string
		everSwitched     bool
		want             bool
	}{
		{
			name:          "first turn of a session never switches",
			decisionModel: "claude-opus-4-7",
			want:          false,
		},
		{
			name:             "steady-state same model, never switched",
			priorServedModel: "claude-opus-4-7",
			decisionModel:    "claude-opus-4-7",
			want:             false,
		},
		{
			name:             "transition turn flips models",
			priorServedModel: "deepseek-v4-pro",
			decisionModel:    "claude-opus-4-7",
			want:             true,
		},
		{
			name:             "switch-back transition turn",
			priorServedModel: "deepseek-v4-pro",
			decisionModel:    "claude-opus-4-7",
			everSwitched:     true,
			want:             true,
		},
		{
			name:             "stay turn after a prior switch still strips",
			priorServedModel: "claude-opus-4-7",
			decisionModel:    "claude-opus-4-7",
			everSwitched:     true,
			want:             true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := turnLoopResult{
				Decision:            router.Decision{Model: tc.decisionModel},
				PriorServedModel:    tc.priorServedModel,
				SessionEverSwitched: tc.everSwitched,
			}
			assert.Equal(t, tc.want, res.modelSwitched())
		})
	}
}
