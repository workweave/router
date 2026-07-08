package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
)

// stubPinStore is a minimal sessionpin.Store for testing recordTurnUsage.
// getPin/getFound configure Get's response and default to a miss.
type stubPinStore struct {
	mu         sync.Mutex
	lastUsage  sessionpin.Usage
	usageHits  int
	usageRoles []string
	getPin     sessionpin.Pin
	getFound   bool
	upserts    []sessionpin.Pin
	upsertErr  error
}

func newStubPinStore() *stubPinStore {
	return &stubPinStore{}
}

func (s *stubPinStore) Get(context.Context, [sessionpin.SessionKeyLen]byte, string) (sessionpin.Pin, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getPin, s.getFound, nil
}

func (s *stubPinStore) Upsert(_ context.Context, p sessionpin.Pin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upserts = append(s.upserts, p)
	return nil
}

func (s *stubPinStore) UpdateUsage(_ context.Context, _ [sessionpin.SessionKeyLen]byte, role string, u sessionpin.Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsage = u
	s.usageHits++
	s.usageRoles = append(s.usageRoles, role)
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
// recordTurnUsage must persist Last* fields in-line so the planner has
// prior-turn evidence by the next turn.
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
	svc.recordTurnUsage(res, res.Decision.Model, 1200, 80, 200, 900)

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

func TestRecordTurnUsage_HMMDecisionWritesHistoryOnly(t *testing.T) {
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
		InstallationID: uuid.New(),
		Decision: router.Decision{
			Provider: "anthropic",
			Model:    "claude-sonnet-5",
			Reason:   "hmm_policy(label=Complex Followup)",
			Metadata: &router.RoutingMetadata{
				Strategy: string(router.StrategyHMM),
				RouteID:  "route-1",
			},
		},
		SessionKey: sessionKey,
		PinRole:    sessionpin.DefaultRole,
		PinTier:    "hmm_fresh_unpinned",
		// Simulate a prior HMM/default-route model so the history role can
		// latch has_ever_switched without mutating the active routing role.
		PriorServedModel: "claude-haiku-4-5",
	}
	svc.recordTurnUsage(res, res.Decision.Model, 1200, 80, 200, 900)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.upserts, 1)
	assert.Equal(t, hmmHistoryRole(sessionpin.DefaultRole), store.upserts[0].Role)
	assert.Equal(t, hmmHistoryReason, store.upserts[0].Reason)
	assert.Empty(t, store.upserts[0].Model, "HMM history rows must not be routable pins")
	assert.Equal(t, []string{
		hmmHistoryRole(sessionpin.DefaultRole),
		hmmHistoryRole(sessionpin.DefaultRole),
	}, store.usageRoles)
	assert.NotContains(t, store.usageRoles, sessionpin.DefaultRole, "HMM turns must not mutate the active routing pin role")
	assert.Equal(t, "claude-sonnet-5", store.lastUsage.ServedModel)
}

// TestLoadPin_DoesNotServeExpiredPostgresPinButKeepsEmitHistory: expired rows
// are routing misses, but has_ever_switched/last_served_model must survive so
// Anthropic emit still strips poisoned thinking blocks.
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

func TestSwitchHistoryFromPins_UsesHMMHistory(t *testing.T) {
	now := time.Now()
	active := sessionpin.Pin{
		LastServedModel: "claude-haiku-4-5",
		LastTurnEndedAt: now.Add(-time.Minute),
	}
	hmmHistory := sessionpin.Pin{
		LastServedModel: "claude-sonnet-5",
		LastTurnEndedAt: now,
	}

	prior, everSwitched := switchHistoryFromPins(active, hmmHistory)

	assert.Equal(t, "claude-sonnet-5", prior)
	assert.True(t, everSwitched, "different active/history models must preserve thinking-block stripping")
}

// TestModelSwitched covers switch → stay → stay: once a session has served
// two models, every later same-model turn must keep stripping stale-signed
// thinking blocks or Anthropic 400s — the has_ever_switched latch.
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
