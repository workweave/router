package hmm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
)

type fakeDecider struct {
	query Query
	res   Result
	err   error
}

func (f *fakeDecider) Decide(_ context.Context, q Query) (Result, error) {
	f.query = q
	return f.res, f.err
}

func TestRouterMapsSidecarRosterModelBackToCatalogDecision(t *testing.T) {
	decider := &fakeDecider{res: Result{
		RouteID:       "route-1",
		Model:         "moonshotai/kimi-k2.7-code",
		Score:         0.8,
		Reason:        "policy",
		Propensity:    0.9,
		DisplayMarker: "display marker",
		PolicyLabel:   "short_turn",
		PolicyGroup:   "standard",
	}}
	deployed := map[string]struct{}{"moonshotai/kimi-k2.7": {}}
	available := map[string]struct{}{"fireworks": {}}
	r := New(decider, deployed, available)

	decision, err := r.Route(context.Background(), router.Request{
		PromptText: "hello",
		ConversationMessages: []router.ConversationMessage{{
			Role: "user",
			Text: "latest hello",
		}},
		EstimatedInputTokens: 10,
	})

	require.NoError(t, err)
	assert.Equal(t, "moonshotai/kimi-k2.7", decision.Model)
	assert.NotNil(t, decision.Metadata)
	assert.Equal(t, "display marker", decision.Metadata.DisplayMarker)
	assert.Equal(t, "route-1", decision.Metadata.RouteID)
	assert.Equal(t, "hmm", decision.Metadata.Strategy)
	assert.Equal(t, float32(0.9), decision.Metadata.Propensity)
	assert.Equal(t, "hello", decider.query.PromptText)
	assert.Equal(t, []router.ConversationMessage{{Role: "user", Text: "latest hello"}}, decider.query.ConversationMessages)
	assert.Equal(t, []Candidate{{RosterID: "moonshotai/kimi-k2.7-code", Provider: "fireworks"}}, decider.query.Candidates)
}

func TestRouterKeepsGeneratedRouteIDWhenSidecarOmitsIt(t *testing.T) {
	decider := &fakeDecider{res: Result{
		Model: "moonshotai/kimi-k2.7-code",
	}}
	r := New(decider, map[string]struct{}{"moonshotai/kimi-k2.7": {}}, map[string]struct{}{"fireworks": {}})

	decision, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.NoError(t, err)
	require.NotNil(t, decision.Metadata)
	assert.NotEmpty(t, decider.query.RouteID)
	assert.Equal(t, decider.query.RouteID, decision.Metadata.RouteID)
}

func TestRouterForwardsHarnessAndPreferenceSignals(t *testing.T) {
	decider := &fakeDecider{res: Result{Model: "moonshotai/kimi-k2.7-code"}}
	r := New(
		decider,
		map[string]struct{}{
			"moonshotai/kimi-k2.7": {},
			"claude-sonnet-5":      {},
		},
		map[string]struct{}{
			providers.ProviderFireworks: {},
			providers.ProviderAnthropic: {},
		},
	)
	alpha := 0.25
	qualityBias := 0.9

	_, err := r.Route(context.Background(), router.Request{
		PromptText:      "hello",
		Harness:         router.HarnessClaudeCode,
		PreferredModels: []string{"moonshotai/kimi-k2.7"},
		ExcludedModels: map[string]struct{}{
			"claude-sonnet-5": {},
		},
		RoutingKnobs: &router.Overrides{
			Alpha:       &alpha,
			QualityBias: &qualityBias,
		},
		SubsidizedModelCostFactor: map[string]float64{
			"moonshotai/kimi-k2.7": 0.15,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, router.HarnessClaudeCode, decider.query.Harness)
	require.NotNil(t, decider.query.RoutingPreferences)
	require.NotNil(t, decider.query.RoutingPreferences.Alpha)
	require.NotNil(t, decider.query.RoutingPreferences.QualityBias)
	assert.Equal(t, alpha, *decider.query.RoutingPreferences.Alpha)
	assert.Equal(t, qualityBias, *decider.query.RoutingPreferences.QualityBias)
	assert.Equal(t, []string{"moonshotai/kimi-k2.7-code"}, decider.query.RoutingPreferences.PreferredModels)
	assert.Equal(t, []string{"anthropic/claude-sonnet-5"}, decider.query.RoutingPreferences.ExcludedModels)
	assert.Equal(t, map[string]float64{"moonshotai/kimi-k2.7-code": 0.15}, decider.query.RoutingPreferences.SubsidizedModelCostFactor)
}

func TestRosterIDsForCatalogIDSetIsDeterministic(t *testing.T) {
	ids := map[string]struct{}{
		"moonshotai/kimi-k2.7": {},
		"claude-sonnet-5":      {},
	}

	for range 10 {
		assert.Equal(t,
			[]string{"anthropic/claude-sonnet-5", "moonshotai/kimi-k2.7-code"},
			rosterIDsForCatalogIDSet(ids),
		)
	}
}

func TestRouterFailsClosedOnUnknownReturnedModel(t *testing.T) {
	decider := &fakeDecider{res: Result{Model: "unknown/model"}}
	r := New(decider, map[string]struct{}{"moonshotai/kimi-k2.7": {}}, map[string]struct{}{"fireworks": {}})

	_, err := r.Route(context.Background(), router.Request{PromptText: "hello"})

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHMMUnavailable)
}

func TestRosterIDForSkipsAmbiguousBareProviderIDs(t *testing.T) {
	got := rosterIDFor(catalog.Model{
		ID: "bare-provider-model",
		Providers: []catalog.ProviderBinding{{
			Provider: providers.ProviderFireworks,
		}},
	})

	assert.Empty(t, got)
}
