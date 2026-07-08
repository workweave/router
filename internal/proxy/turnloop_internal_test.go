package proxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/planner"
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
	usageErrs  []error
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
	if len(s.usageErrs) > 0 {
		err := s.usageErrs[0]
		s.usageErrs = s.usageErrs[1:]
		if err != nil {
			return err
		}
	}
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
	svc.recordTurnUsage(res, res.Decision.Provider, res.Decision.Model, 1200, 80, 200, 900)

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
	svc.recordTurnUsage(res, res.Decision.Provider, res.Decision.Model, 1200, 80, 200, 900)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.upserts, 1)
	assert.Equal(t, hmmHistoryRole(sessionpin.DefaultRole), store.upserts[0].Role)
	assert.Equal(t, "hmm_policy(label=Complex Followup)", store.upserts[0].Reason)
	assert.Equal(t, providers.ProviderAnthropic, store.upserts[0].Provider)
	assert.Empty(t, store.upserts[0].Model, "HMM history rows must not be routable pins")
	assert.Equal(t, []string{
		hmmHistoryRole(sessionpin.DefaultRole),
		hmmHistoryRole(sessionpin.DefaultRole),
	}, store.usageRoles)
	assert.NotContains(t, store.usageRoles, sessionpin.DefaultRole, "HMM turns must not mutate the active routing pin role")
	assert.Equal(t, 1200, store.lastUsage.InputTokens)
	assert.Equal(t, 900, store.lastUsage.CachedReadTokens)
	assert.Equal(t, 200, store.lastUsage.CachedWriteTokens)
	assert.Equal(t, 80, store.lastUsage.OutputTokens)
	assert.Equal(t, "claude-sonnet-5", store.lastUsage.ServedModel)
}

func TestRecordTurnUsage_HMMPriorWritebackErrorStillWritesCurrentUsage(t *testing.T) {
	store := newStubPinStore()
	store.usageErrs = []error{errors.New("transient writeback failure")}
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
			Provider: providers.ProviderAnthropic,
			Model:    "claude-sonnet-5",
			Reason:   "hmm_policy(label=Complex Followup)",
			Metadata: &router.RoutingMetadata{
				Strategy: string(router.StrategyHMM),
				RouteID:  "route-1",
			},
		},
		SessionKey:       sessionKey,
		PinRole:          sessionpin.DefaultRole,
		PinTier:          "hmm_fresh_unpinned",
		PriorServedModel: "claude-haiku-4-5",
	}
	svc.recordTurnUsage(res, res.Decision.Provider, res.Decision.Model, 1200, 80, 200, 900)

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Equal(t, 2, store.usageHits)
	assert.Equal(t, []string{
		hmmHistoryRole(sessionpin.DefaultRole),
		hmmHistoryRole(sessionpin.DefaultRole),
	}, store.usageRoles)
	assert.Equal(t, 1200, store.lastUsage.InputTokens)
	assert.Equal(t, 80, store.lastUsage.OutputTokens)
	assert.Equal(t, "claude-sonnet-5", store.lastUsage.ServedModel)
}

func TestRecordHMMTurnHistory_ZeroUsageRefreshesTTLButSkipsUsageWriteback(t *testing.T) {
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
			Provider: providers.ProviderAnthropic,
			Model:    "claude-sonnet-5",
			Reason:   "hmm_policy(label=Complex Followup)",
			Metadata: &router.RoutingMetadata{
				Strategy: string(router.StrategyHMM),
				RouteID:  "route-1",
			},
		},
		SessionKey: sessionKey,
		PinRole:    sessionpin.DefaultRole,
	}
	// A failed/empty upstream turn: all usage counts zero.
	svc.recordTurnUsage(res, res.Decision.Provider, res.Decision.Model, 0, 0, 0, 0)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.upserts, 1, "TTL-refreshing upsert must still run on a zero-usage turn")
	assert.Equal(t, hmmHistoryRole(sessionpin.DefaultRole), store.upserts[0].Role)
	assert.Empty(t, store.usageRoles, "zero-usage turn must not clobber the history row's usage columns")
	assert.Equal(t, 0, store.usageHits)
}

func TestRecordTurnUsage_HMMEVStayWritesHistoryOnly(t *testing.T) {
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
			Provider: providers.ProviderAnthropic,
			Model:    "claude-sonnet-5",
			Reason:   hmmHistoryReason,
		},
		Fresh: router.Decision{
			Provider: providers.ProviderMakora,
			Model:    "deepseek/deepseek-v4-flash",
			Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
			Metadata: &router.RoutingMetadata{
				Strategy: string(router.StrategyHMM),
				RouteID:  "route-1",
			},
		},
		SessionKey: sessionKey,
		PinRole:    sessionpin.DefaultRole,
		PinTier:    "hmm_ev_stay_ev_negative",
	}
	svc.recordTurnUsage(res, res.Decision.Provider, res.Decision.Model, 1200, 80, 200, 900)

	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.upserts, 1)
	assert.Equal(t, hmmHistoryRole(sessionpin.DefaultRole), store.upserts[0].Role)
	assert.Empty(t, store.upserts[0].Model, "HMM history rows must not be routable pins")
	assert.Equal(t, []string{hmmHistoryRole(sessionpin.DefaultRole)}, store.usageRoles)
	assert.Equal(t, 80, store.lastUsage.OutputTokens)
	assert.Equal(t, "claude-sonnet-5", store.lastUsage.ServedModel)
}

func TestStickyStateRole_HMMEVStayTargetsHistory(t *testing.T) {
	res := turnLoopResult{
		StickyHit:  true,
		PinRole:    sessionpin.DefaultRole,
		StickyRole: hmmHistoryRole(sessionpin.DefaultRole),
	}
	assert.Equal(t, hmmHistoryRole(sessionpin.DefaultRole), stickyStateRole(res))
}

func TestStickyStateRole_DefaultsToActivePinRole(t *testing.T) {
	res := turnLoopResult{
		StickyHit: true,
		PinRole:   sessionpin.DefaultRole,
	}
	assert.Equal(t, sessionpin.DefaultRole, stickyStateRole(res))
}

func TestHMMCostGate_StaysOnWarmCacheWhenCheaperFreshDoesNotClearEV(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	history := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		LastServedModel: "claude-sonnet-5",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		100,
		false,
	)

	assert.True(t, sticky)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
	assert.Equal(t, "claude-sonnet-5", stayModel)
	assert.Equal(t, planner.OutcomeStay, plan.Outcome)
	assert.Equal(t, planner.ReasonEVNegative, plan.Reason)
}

func TestHMMCostGate_SwitchesCheaperFreshWhenEVPositive(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	history := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		LastServedModel: "claude-sonnet-5",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		10_000,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Equal(t, "claude-sonnet-5", stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, planner.ReasonEVPositive, plan.Reason)
}

func TestHMMCostGate_PhaseChangeFollowsFreshDecision(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	activePin := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		LastServedModel: "claude-sonnet-5",
		Reason:          "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
		PinnedUntil:     time.Now().Add(time.Hour),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Followup')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		activePin,
		sessionpin.Pin{},
		fresh,
		100,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Equal(t, "claude-sonnet-5", stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, hmmReasonPhaseChange, plan.Reason)
}

func TestHMMCostGate_HistoryPhaseChangeFollowsFreshDecision(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	history := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		LastServedModel: "claude-sonnet-5",
		Reason:          "hmm_policy:tool_execution(label=SPAWN_EXPLORE)",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
		PinnedUntil:     time.Now().Add(time.Hour),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Followup')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		100,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Equal(t, "claude-sonnet-5", stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, hmmReasonPhaseChange, plan.Reason)
}

func TestHMMCostGate_ExpensiveUpgradeRequiresHighConfidence(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderFireworks: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	history := sessionpin.Pin{
		Provider:        providers.ProviderFireworks,
		LastServedModel: "moonshotai/kimi-k2.7",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fresh := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Reason:   "hmm_policy(classifier 'Complex Followup')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.84,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		10_000,
		false,
	)

	assert.True(t, sticky)
	assert.Equal(t, "moonshotai/kimi-k2.7", decision.Model)
	assert.Equal(t, "moonshotai/kimi-k2.7", stayModel)
	assert.Equal(t, planner.OutcomeStay, plan.Outcome)
	assert.Equal(t, hmmReasonUpgradeConfidenceLow, plan.Reason)

	fresh.Metadata.ChosenScore = 0.85
	decision, plan, sticky, stayModel = svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		10_000,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, "moonshotai/kimi-k2.7", stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, hmmReasonConfidentUpgrade, plan.Reason)
}

func TestHMMCostGate_LowConfidenceUpgradeKeepsIndependentPlannerSwitch(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderFireworks: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	).WithPlanner(planner.EVConfig{
		ThresholdUSD:           DefaultPlannerThresholdUSD,
		ExpectedRemainingTurns: DefaultPlannerExpectedRemainingTurns,
		TierUpgradeEnabled:     false,
		ColdPinFollowFresh:     true,
	})
	history := sessionpin.Pin{
		Provider:        providers.ProviderFireworks,
		LastServedModel: "moonshotai/kimi-k2.7",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
	}
	fresh := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    "claude-sonnet-5",
		Reason:   "hmm_policy(classifier 'Complex Followup')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.84,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		10_000,
		true,
	)

	assert.False(t, sticky)
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, "moonshotai/kimi-k2.7", stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, planner.ReasonColdPinFresh, plan.Reason)
}

func TestHMMCostGate_IgnoresExpiredActivePin(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	expired := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		LastServedModel: "claude-sonnet-5",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
		PinnedUntil:     time.Now().Add(-time.Minute),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		expired,
		sessionpin.Pin{},
		fresh,
		100,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Empty(t, stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, planner.ReasonNoPin, plan.Reason)
}

func TestHMMCostGate_IgnoresNonHMMActivePin(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	// A warm cluster/planner pin (non-HMM reason) must NOT steer an HMM turn's
	// EV stay — otherwise HMM turns silently reuse ordinary session pins.
	clusterPin := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		LastServedModel: "claude-sonnet-5",
		Reason:          "cluster:v0.2",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
		PinnedUntil:     time.Now().Add(time.Hour),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		clusterPin,
		sessionpin.Pin{},
		fresh,
		100,
		false,
	)

	assert.False(t, sticky, "a non-HMM cluster pin must not win an HMM EV stay")
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Empty(t, stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, planner.ReasonNoPin, plan.Reason)
}

func TestHMMCostGate_HonorsHMMReasonedActivePin(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	// The active pin IS HMM-reasoned, so it remains a valid stay candidate: a
	// cheaper fresh pick that doesn't clear EV must stay on it.
	hmmPin := sessionpin.Pin{
		Provider:        providers.ProviderAnthropic,
		Model:           "claude-sonnet-5",
		LastServedModel: "claude-sonnet-5",
		Reason:          "hmm_policy(label=Complex Followup)",
		LastTurnEndedAt: time.Now().Add(-30 * time.Second),
		PinnedUntil:     time.Now().Add(time.Hour),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		hmmPin,
		sessionpin.Pin{},
		fresh,
		100,
		false,
	)

	assert.True(t, sticky, "an HMM-reasoned active pin remains a valid stay candidate")
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, "claude-sonnet-5", stayModel)
	assert.Equal(t, planner.OutcomeStay, plan.Outcome)
	assert.Equal(t, planner.ReasonEVNegative, plan.Reason)
}

func TestHMMCostGate_IgnoresMaxedHistory(t *testing.T) {
	svc := NewService(
		nil,
		map[string]providers.Client{providers.ProviderAnthropic: nil, providers.ProviderMakora: nil},
		nil,
		false,
		nil,
		nil,
		false,
		"anthropic", "claude-haiku-4-5",
		nil,
	)
	history := sessionpin.Pin{
		Provider:         providers.ProviderAnthropic,
		LastServedModel:  "claude-sonnet-5",
		LastOutputTokens: prevTurnMaxedOutThreshold,
		LastTurnEndedAt:  time.Now().Add(-30 * time.Second),
		PinnedUntil:      time.Now().Add(time.Hour),
	}
	fresh := router.Decision{
		Provider: providers.ProviderMakora,
		Model:    "deepseek/deepseek-v4-flash",
		Reason:   "hmm_policy(classifier 'Simple Tool Call Request')",
		Metadata: &router.RoutingMetadata{
			Strategy:    string(router.StrategyHMM),
			RouteID:     "route-1",
			ChosenScore: 0.70,
		},
	}

	decision, plan, sticky, stayModel := svc.hmmCostGatedDecision(
		router.Request{},
		sessionpin.Pin{},
		history,
		fresh,
		100,
		false,
	)

	assert.False(t, sticky)
	assert.Equal(t, "deepseek/deepseek-v4-flash", decision.Model)
	assert.Empty(t, stayModel)
	assert.Equal(t, planner.OutcomeSwitch, plan.Outcome)
	assert.Equal(t, planner.ReasonNoPin, plan.Reason)
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
