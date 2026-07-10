package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
)

type recordingPolicy struct {
	query    policy.Query
	result   policy.Result
	outcome  map[string]interface{}
	feedback map[string]interface{}
}

func (p *recordingPolicy) Decide(_ context.Context, query policy.Query) (policy.Result, error) {
	p.query = query
	return p.result, nil
}

func (p *recordingPolicy) ReportOutcome(_ context.Context, payload map[string]interface{}) error {
	p.outcome = payload
	return nil
}

func (p *recordingPolicy) ReportFeedback(_ context.Context, payload map[string]interface{}) error {
	p.feedback = payload
	return nil
}

func TestSidecarRouterOnboardsFutureStrategyWithoutProxyChanges(t *testing.T) {
	strategy := router.Strategy("future-policy")
	decider := &recordingPolicy{result: policy.Result{
		SchemaVersion:        policy.SchemaVersionV1,
		RouteID:              "route-future",
		Model:                "future/gpt-5.5",
		Provider:             providers.ProviderOpenAI,
		Score:                0.9,
		CandidateScores:      map[string]float32{"future/gpt-5.5": 0.9},
		PolicyRouteKey:       "high",
		PolicyArtifactID:     "future-prod",
		PolicyArtifactSHA256: "sha256:future",
		RosterVersion:        "roster-1",
		DebugRef:             "must-not-leak",
	}}
	resolver := policy.NewResolver(
		set("gpt-5.5"),
		set(providers.ProviderOpenAI),
		func(model catalog.Model) string { return "future/" + model.ID },
		policy.ManagedProviderPolicy(),
	)
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy:    strategy,
		Unavailable: errors.New("future unavailable"),
	}, decider, resolver)

	decision, err := adapter.Route(context.Background(), router.Request{
		OrganizationID:       "org-1",
		InstallationID:       "installation-1",
		ClientApp:            "cursor",
		RoutingIntent:        "high",
		TrainingAllowed:      true,
		CaptureMode:          "hashed",
		EstimatedInputTokens: 1000,
	})

	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", decision.Model)
	assert.Equal(t, providers.ProviderOpenAI, decision.Provider)
	assert.Equal(t, "future-policy", decision.Metadata.Strategy)
	assert.Equal(t, "route-future", decision.Metadata.RouteID)
	assert.Equal(t, "high", decision.Metadata.PolicyRouteKey)
	assert.Equal(t, "future-prod", decision.Metadata.PolicyArtifactID)
	assert.Equal(t, map[string]float32{"gpt-5.5": 0.9}, decision.Metadata.CandidateScores)
	assert.Empty(t, decision.Metadata.DebugRef)
	assert.Equal(t, strategy, decider.query.Strategy)
	assert.Equal(t, "org-1", decider.query.OrganizationID)
	assert.Equal(t, "cursor", decider.query.ClientApp)
	assert.True(t, decider.query.TrainingAllowed)
	assert.Equal(t, "hashed", decider.query.CaptureMode)
	require.Len(t, decider.query.Candidates, 1)
	assert.Equal(t, "future/gpt-5.5", decider.query.Candidates[0].RosterID)
	assert.Greater(t, decider.query.Candidates[0].InputUSDPer1M, 0.0)
	assert.Greater(t, decider.query.Candidates[0].Capabilities.ContextWindow, 0)

	require.NoError(t, adapter.ReportOutcome(context.Background(), map[string]interface{}{"route_id": "route-future"}))
	require.NoError(t, adapter.ReportFeedback(context.Background(), map[string]interface{}{"feedback": "positive"}))
	assert.Equal(t, "route-future", decider.outcome["route_id"])
	assert.Equal(t, "positive", decider.feedback["feedback"])
}
