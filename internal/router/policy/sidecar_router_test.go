package policy_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
)

type recordingPolicy struct {
	query        policy.Query
	previewQuery policy.Query
	result       policy.Result
	preview      policy.PreviewResult
	outcome      map[string]interface{}
	feedback     map[string]interface{}
}

func (p *recordingPolicy) Decide(_ context.Context, query policy.Query) (policy.Result, error) {
	p.query = query
	return p.result, nil
}

func (p *recordingPolicy) Preview(_ context.Context, query policy.Query) (policy.PreviewResult, error) {
	p.previewQuery = query
	return p.preview, nil
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
	}, decider, resolver).WithCapabilities(policy.Capabilities{
		SchemaVersion:                 policy.SchemaVersionV1,
		AuthoritativePerTurnSelection: true,
		ReportsOutcomes:               true,
		ReportsFeedback:               true,
	})
	priorOutputTokens := 42

	decision, err := adapter.Route(context.Background(), router.Request{
		OrganizationID:       "org-1",
		InstallationID:       "installation-1",
		ClientApp:            "cursor",
		RoutingIntent:        "high",
		TrainingAllowed:      true,
		CaptureMode:          "hashed",
		EstimatedInputTokens: 1000,
		PolicyTurnContext: &router.PolicyTurnContext{
			VisibleTurnIndex:    3,
			SessionTurnCount:    4,
			TurnType:            "main_loop",
			PreviousServedModel: "gpt-5.4",
			PreviousProvider:    providers.ProviderOpenAI,
			CacheState:          router.PolicyCacheStateWarm,
			PriorOutputTokens:   &priorOutputTokens,
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", decision.Model)
	assert.Equal(t, providers.ProviderOpenAI, decision.Provider)
	assert.Equal(t, "future-policy", decision.Metadata.Strategy)
	assert.Equal(t, "route-future", decision.Metadata.RouteID)
	assert.Equal(t, "high", decision.Metadata.PolicyRouteKey)
	assert.Equal(t, "future-prod", decision.Metadata.PolicyArtifactID)
	assert.Equal(t, map[string]float32{"gpt-5.5": 0.9}, decision.Metadata.CandidateScores)
	assert.True(t, decision.Metadata.AuthoritativePerTurnSelection)
	assert.Empty(t, decision.Metadata.DebugRef)
	assert.Equal(t, strategy, decider.query.Strategy)
	assert.Equal(t, policy.ExecutionModeServing, decider.query.ExecutionMode)
	assert.Equal(t, "org-1", decider.query.OrganizationID)
	assert.Equal(t, "cursor", decider.query.ClientApp)
	assert.True(t, decider.query.TrainingAllowed)
	assert.Equal(t, "hashed", decider.query.CaptureMode)
	require.NotNil(t, decider.query.TurnContext)
	assert.Equal(t, 3, decider.query.TurnContext.VisibleTurnIndex)
	assert.Equal(t, "gpt-5.4", decider.query.TurnContext.PreviousServedModel)
	require.Len(t, decider.query.Candidates, 1)
	assert.Equal(t, "future/gpt-5.5", decider.query.Candidates[0].RosterID)
	assert.Greater(t, decider.query.Candidates[0].InputUSDPer1M, 0.0)
	assert.Greater(t, decider.query.Candidates[0].Capabilities.ContextWindow, 0)

	require.NoError(t, adapter.ReportOutcome(context.Background(), map[string]interface{}{"route_id": "route-future"}))
	require.NoError(t, adapter.ReportFeedback(context.Background(), map[string]interface{}{"feedback": "positive"}))
	assert.Equal(t, "route-future", decider.outcome["route_id"])
	assert.Equal(t, "positive", decider.feedback["feedback"])
}

func TestSidecarRouterDispatchesSidecarSelectedArm(t *testing.T) {
	resolver := policy.NewArmResolver(
		set("deepseek/deepseek-v4-pro"),
		set(providers.ProviderMakora, providers.ProviderFireworks),
		func(model catalog.Model) string { return model.ID },
		policy.ManagedProviderPolicy(),
	)
	resolved := resolver.Resolve(router.Request{})
	require.Len(t, resolved.Candidates, 2)
	selected := resolved.Candidates[1]
	decider := &recordingPolicy{result: policy.Result{
		ArmID:    selected.ArmID,
		Provider: selected.Provider,
		Score:    0.9,
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, decider, resolver)

	decision, err := adapter.Route(context.Background(), router.Request{})

	require.NoError(t, err)
	assert.Equal(t, selected.CatalogID, decision.Model)
	assert.Equal(t, selected.Provider, decision.Provider)
	assert.Equal(t, selected.ArmID, decision.Metadata.SelectedArmID)
}

func TestSidecarRouterMarksShadowDecisionsNonLearning(t *testing.T) {
	decider := &recordingPolicy{result: policy.Result{Model: "gpt-5.5"}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, decider, policy.NewResolver(
		set("gpt-5.5"),
		set(providers.ProviderOpenAI),
		func(model catalog.Model) string { return model.ID },
		policy.ManagedProviderPolicy(),
	))

	_, err := adapter.Route(context.Background(), router.Request{
		ShadowMode: true, TrainingAllowed: true, DebugEnabled: true,
	})

	require.NoError(t, err)
	assert.Equal(t, policy.ExecutionModeShadow, decider.query.ExecutionMode)
	assert.False(t, decider.query.TrainingAllowed)
	assert.False(t, decider.query.DebugEnabled)
}

func TestSidecarRouterPreviewReturnsAllEligibleArmsWithoutLifecycleCallbacks(t *testing.T) {
	decider := &recordingPolicy{preview: policy.PreviewResult{
		SchemaVersion:         policy.SchemaVersionV1,
		PolicyArtifactID:      "hmm-prod",
		PolicyArtifactSHA256:  "sha256:artifact",
		RosterSHA256:          "sha256:roster",
		HMMStateID:            7,
		HMMStatePath:          []int{2, 7},
		HMMStateProbabilities: []float64{0.01, 0.01, 0.05, 0.01, 0.01, 0.01, 0.1, 0.8},
		ClassOrder:            []string{"hard", "balanced"},
		ClassProbabilities:    map[string]float64{"hard": 0.8, "balanced": 0.2},
		RankedFallback: []policy.PreviewGroup{{
			Group:        "hard",
			Probability:  0.8,
			RosterArms:   []string{"claude-opus-4-8", "gpt-5.5"},
			EligibleArms: []string{"claude-opus-4-8", "gpt-5.5"},
		}, {
			Group:       "balanced",
			Probability: 0.2,
		}},
		SelectedGroup:     "hard",
		EligibleRosterIDs: []string{"claude-opus-4-8", "gpt-5.5"},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, policy.NewResolver(
		set("claude-opus-4-8", "gpt-5.5"),
		set(providers.ProviderAnthropic, providers.ProviderOpenAI),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)).WithCapabilities(policy.Capabilities{SupportsPreview: true})

	result, err := adapter.PreviewRoute(context.Background(), router.Request{
		OrganizationID:  "org-1",
		InstallationID:  "installation-1",
		TrainingAllowed: true,
		DebugEnabled:    false,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"claude-opus-4-8", "gpt-5.5"}, result.EligibleRosterIDs)
	assert.Equal(t, router.StrategyHMM, result.Strategy)
	assert.NotEmpty(t, result.RouteID)
	assert.Equal(t, policy.ExecutionModePreview, decider.previewQuery.ExecutionMode)
	assert.False(t, decider.previewQuery.TrainingAllowed)
	assert.True(t, decider.previewQuery.DebugEnabled)
	assert.Len(t, decider.previewQuery.Candidates, 2)
	assert.Nil(t, decider.outcome)
	assert.Nil(t, decider.feedback)
}

func TestSidecarRouterPreviewRecordsZeroEligibleArms(t *testing.T) {
	decider := &recordingPolicy{preview: policy.PreviewResult{
		SchemaVersion:         policy.SchemaVersionV1,
		PolicyArtifactID:      "hmm-prod",
		PolicyArtifactSHA256:  "sha256:artifact",
		RosterSHA256:          "sha256:roster",
		HMMStateID:            1,
		HMMStatePath:          []int{1},
		HMMStateProbabilities: []float64{0.2, 0.8},
		ClassOrder:            []string{"hard"},
		ClassProbabilities:    map[string]float64{"hard": 1},
		RankedFallback: []policy.PreviewGroup{{
			Group:       "hard",
			Probability: 1,
		}},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, policy.NewResolver(
		set("gpt-5.5"),
		set(providers.ProviderOpenAI),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)).WithCapabilities(policy.Capabilities{SupportsPreview: true})

	result, err := adapter.PreviewRoute(context.Background(), router.Request{
		ExcludedModels: set("gpt-5.5"),
	})

	require.NoError(t, err)
	assert.Empty(t, result.EligibleRosterIDs)
	assert.Empty(t, result.ResolverCandidates)
	assert.Contains(t, result.ResolverExclusions, policy.Diagnostic{
		CatalogID: "gpt-5.5",
		Reason:    policy.ExclusionRequested,
	})
	assert.Empty(t, result.SelectedGroup)
	assert.Empty(t, decider.previewQuery.Candidates)
}

func TestSidecarRouterPreviewRejectsUnknownArm(t *testing.T) {
	decider := &recordingPolicy{preview: policy.PreviewResult{
		SchemaVersion:         policy.SchemaVersionV1,
		PolicyArtifactID:      "hmm-prod",
		PolicyArtifactSHA256:  "sha256:artifact",
		RosterSHA256:          "sha256:roster",
		HMMStateID:            1,
		HMMStatePath:          []int{1},
		HMMStateProbabilities: []float64{0.2, 0.8},
		ClassOrder:            []string{"hard"},
		ClassProbabilities:    map[string]float64{"hard": 1},
		RankedFallback: []policy.PreviewGroup{{
			Group:        "hard",
			Probability:  1,
			RosterArms:   []string{"unoffered-model"},
			EligibleArms: []string{"unoffered-model"},
		}},
		SelectedGroup:     "hard",
		EligibleRosterIDs: []string{"unoffered-model"},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, policy.NewResolver(
		set("gpt-5.5"),
		set(providers.ProviderOpenAI),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	))

	_, err := adapter.PreviewRoute(context.Background(), router.Request{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown roster id")
}

func TestSidecarRouterCapabilitiesRejectUnsupportedShadow(t *testing.T) {
	unavailable := errors.New("future unavailable")
	decider := &recordingPolicy{}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy:    router.Strategy("future-policy"),
		Unavailable: unavailable,
	}, decider, nil).WithCapabilities(policy.Capabilities{})

	_, err := adapter.Route(context.Background(), router.Request{ShadowMode: true})

	require.ErrorIs(t, err, unavailable)
	assert.Empty(t, decider.query.ExecutionMode)
}

func TestSidecarRouterCapabilitiesGateOptionalLifecycleCalls(t *testing.T) {
	decider := &recordingPolicy{}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, decider, nil).WithCapabilities(policy.Capabilities{})

	require.NoError(t, adapter.ReportOutcome(context.Background(), map[string]interface{}{"route_id": "route-1"}))
	require.NoError(t, adapter.ReportFeedback(context.Background(), map[string]interface{}{"feedback": "positive"}))
	assert.Nil(t, decider.outcome)
	assert.Nil(t, decider.feedback)
}

func TestSidecarRouterCapabilitiesCanRefreshAfterStartup(t *testing.T) {
	decider := &recordingPolicy{}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, decider, nil).WithCapabilities(policy.Capabilities{})

	adapter.WithCapabilities(policy.Capabilities{
		SchemaVersion:   policy.SchemaVersionV1,
		ReportsOutcomes: true,
		ReportsFeedback: true,
		SupportsShadow:  true,
	})

	require.NoError(t, adapter.ReportOutcome(context.Background(), map[string]interface{}{"route_id": "route-2"}))
	require.NoError(t, adapter.ReportFeedback(context.Background(), map[string]interface{}{"feedback": "positive"}))
	assert.Equal(t, "route-2", decider.outcome["route_id"])
	assert.Equal(t, "positive", decider.feedback["feedback"])
	assert.True(t, adapter.CurrentCapabilities().SupportsShadow)
}

func TestSidecarRouterCapabilityRefreshIsConcurrentSafe(t *testing.T) {
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, &recordingPolicy{}, nil).WithCapabilities(policy.Capabilities{})
	var workers sync.WaitGroup

	for i := 0; i < 100; i++ {
		workers.Add(2)
		go func(supportsShadow bool) {
			defer workers.Done()
			adapter.WithCapabilities(policy.Capabilities{SupportsShadow: supportsShadow})
		}(i%2 == 0)
		go func() {
			defer workers.Done()
			_ = adapter.CurrentCapabilities()
		}()
	}

	workers.Wait()
}
