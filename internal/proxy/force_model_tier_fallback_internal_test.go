package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierProbeRouter mimics the production-observed collapse that motivated the
// fix: among the eligible candidates (available models minus ExcludedModels) it
// returns the CHEAPEST one (lowest tier). Without a tier constraint a dropped
// high-tier force-model would therefore route to the low-tier default; with the
// constraint the low-tier candidates are excluded, so it must return a same-tier
// model. It records every request so the constraint itself can be asserted.
type tierProbeRouter struct {
	available map[string]struct{}
	captured  []router.Request
}

func (r *tierProbeRouter) Route(_ context.Context, req router.Request) (router.Decision, error) {
	r.captured = append(r.captured, req)
	best := ""
	bestTier := catalog.TierHigh + 1
	for m := range r.available {
		if _, excluded := req.ExcludedModels[m]; excluded {
			continue
		}
		if t := catalog.TierFor(m); best == "" || t < bestTier {
			best, bestTier = m, t
		}
	}
	if best == "" {
		return router.Decision{}, errors.New("no eligible candidate")
	}
	return router.Decision{Provider: providers.ProviderAnthropic, Model: best, Reason: "fake"}, nil
}

// forcedPinStore returns a single user-forced pin for every lookup.
type forcedPinStore struct {
	pin sessionpin.Pin
}

func (s *forcedPinStore) Get(_ context.Context, key [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	p := s.pin
	p.SessionKey = key
	p.Role = role
	return p, true, nil
}
func (s *forcedPinStore) Upsert(context.Context, sessionpin.Pin) error { return nil }
func (s *forcedPinStore) UpdateUsage(context.Context, [sessionpin.SessionKeyLen]byte, string, sessionpin.Usage) error {
	return nil
}
func (s *forcedPinStore) IncrementUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) (int, error) {
	return 0, nil
}
func (s *forcedPinStore) ResetUpstreamErrors(context.Context, [sessionpin.SessionKeyLen]byte, string) error {
	return nil
}
func (s *forcedPinStore) SweepExpired(context.Context) error { return nil }

// TestRunTurnLoop_ForcedModelContextOverflow_StaysInTier is the regression for
// the debug-session bug: a user force-pinned deepseek-v4-pro (TierHigh) was
// evicted by the context-window pre-filter and the turn collapsed all the way
// to haiku (TierLow) instead of the next-best same-tier model. The fix
// constrains the fresh decision to the forced model's tier, so the evicted
// high-tier pin reroutes to a high-tier model (here, Opus).
func TestRunTurnLoop_ForcedModelContextOverflow_StaysInTier(t *testing.T) {
	const forced = "deepseek/deepseek-v4-pro"
	require.Equal(t, catalog.TierHigh, catalog.TierFor(forced), "test premise: forced model is high-tier")
	require.Equal(t, catalog.TierLow, catalog.TierFor("claude-haiku-4-5"), "test premise: haiku is low-tier")
	require.Equal(t, catalog.TierHigh, catalog.TierFor("claude-opus-4-8"), "test premise: opus is high-tier")

	fr := &tierProbeRouter{available: map[string]struct{}{
		forced:             {},
		"claude-opus-4-8":  {},
		"claude-haiku-4-5": {},
	}}
	store := &forcedPinStore{pin: sessionpin.Pin{
		Provider:    providers.ProviderFireworks,
		Model:       forced,
		Reason:      translate.ReasonUserForceModel,
		PinnedUntil: time.Now().Add(time.Hour),
	}}
	svc := NewService(fr, nil, nil, false, nil, store, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithAvailableModels(fr.available).
		WithPlannerEnabled(false)

	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	feats := env.RoutingFeatures(false)

	// The context-window pre-filter would add the forced model to ExcludedModels
	// on the turn its window is breached; simulate that here.
	res, err := svc.runTurnLoop(context.Background(), env, feats, "key-1", uuid.New(), "", nil, router.Request{
		RequestedModel: feats.Model,
		ExcludedModels: map[string]struct{}{forced: {}},
	})
	require.NoError(t, err)

	assert.Equal(t, "claude-opus-4-8", res.Decision.Model,
		"evicted high-tier force-model must reroute to the next-best same-tier model, not collapse to low-tier")
	assert.Equal(t, catalog.TierHigh, catalog.TierFor(res.Decision.Model),
		"replacement must share the forced model's tier")

	// The scorer must have been handed a tier-constrained denylist: the
	// low-tier candidate is excluded so it can never be chosen.
	require.Len(t, fr.captured, 1, "exactly one (constrained) scorer call")
	_, haikuExcluded := fr.captured[0].ExcludedModels["claude-haiku-4-5"]
	assert.True(t, haikuExcluded, "tier constraint must exclude the low-tier model from the scorer pool")
	_, opusExcluded := fr.captured[0].ExcludedModels["claude-opus-4-8"]
	assert.False(t, opusExcluded, "the same-tier replacement must remain eligible")
}

// TestRestrictToTier_FallsBackWhenNoInTierCandidate guards the empty-pool
// escape hatch: when no in-tier model would survive the constraint, the helper
// returns the original denylist and ok=false so the caller leaves routing
// unconstrained rather than handing the scorer an empty pool.
func TestRestrictToTier_FallsBackWhenNoInTierCandidate(t *testing.T) {
	svc := NewService(nil, nil, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithAvailableModels(map[string]struct{}{"claude-haiku-4-5": {}})

	excluded := map[string]struct{}{"deepseek/deepseek-v4-pro": {}}
	out, ok := svc.restrictToTier(excluded, catalog.TierHigh)
	assert.False(t, ok, "no high-tier model is available, so the constraint must not apply")
	assert.Equal(t, excluded, out, "the original denylist is returned unchanged on fallback")
}

// TestRestrictToTier_ExcludesOtherTiers verifies the constraint set: every
// available model outside the target tier is added to the denylist while
// in-tier models stay eligible.
func TestRestrictToTier_ExcludesOtherTiers(t *testing.T) {
	svc := NewService(nil, nil, nil, false, nil, nil, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil).
		WithAvailableModels(map[string]struct{}{
			"claude-opus-4-8":   {}, // high
			"claude-haiku-4-5":  {}, // low
			"claude-sonnet-4-6": {}, // mid
		})

	out, ok := svc.restrictToTier(nil, catalog.TierHigh)
	require.True(t, ok)
	_, haikuExcluded := out["claude-haiku-4-5"]
	_, sonnetExcluded := out["claude-sonnet-4-6"]
	_, opusExcluded := out["claude-opus-4-8"]
	assert.True(t, haikuExcluded, "low-tier excluded")
	assert.True(t, sonnetExcluded, "mid-tier excluded")
	assert.False(t, opusExcluded, "high-tier stays eligible")
}
