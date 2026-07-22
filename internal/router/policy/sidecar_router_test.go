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
	assert.Equal(t, policy.SchemaVersionV1, decider.query.SchemaVersion)
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
		CandidateScores: map[string]float32{
			resolved.Candidates[0].ArmID: 0.1,
			resolved.Candidates[1].ArmID: 0.9,
		},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("future-policy"),
	}, decider, resolver)

	decision, err := adapter.Route(context.Background(), router.Request{})

	require.NoError(t, err)
	assert.Equal(t, selected.CatalogID, decision.Model)
	assert.Equal(t, selected.Provider, decision.Provider)
	assert.Equal(t, selected.ArmID, decision.Metadata.SelectedArmID)
	assert.Equal(t, map[string]string{
		resolved.Candidates[0].ArmID: resolved.Candidates[0].Provider,
		resolved.Candidates[1].ArmID: resolved.Candidates[1].Provider,
	}, decision.Metadata.CandidateArmProviders)
	assert.Equal(t, map[string]float32{
		resolved.Candidates[0].ArmID: 0.1,
		resolved.Candidates[1].ArmID: 0.9,
	}, decision.Metadata.CandidateArmScores)
	assert.Equal(t, policy.SchemaVersionV2, decider.query.SchemaVersion)
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
	assert.Equal(t, policy.SchemaVersionV1, decider.previewQuery.SchemaVersion)
	assert.Equal(t, policy.ExecutionModePreview, decider.previewQuery.ExecutionMode)
	assert.False(t, decider.previewQuery.TrainingAllowed)
	assert.True(t, decider.previewQuery.DebugEnabled)
	assert.Len(t, decider.previewQuery.Candidates, 2)
	assert.Nil(t, decider.outcome)
	assert.Nil(t, decider.feedback)
}

func TestSidecarRouterPreviewUsesArmSchemaAndIDs(t *testing.T) {
	resolver := policy.NewArmResolver(
		set("deepseek/deepseek-v4-pro"),
		set(providers.ProviderMakora, providers.ProviderFireworks),
		catalogRosterID,
		policy.ManagedProviderPolicy(),
	)
	resolved := resolver.Resolve(router.Request{})
	require.Len(t, resolved.Candidates, 2)
	selectedArmID := resolved.Candidates[0].ArmID
	decider := &recordingPolicy{preview: policy.PreviewResult{
		SchemaVersion:         policy.SchemaVersionV2,
		PolicyArtifactID:      "temporal-q-v1",
		PolicyArtifactSHA256:  "sha256:artifact",
		RosterSHA256:          "sha256:roster",
		HMMStateID:            0,
		HMMStatePath:          []int{0},
		HMMStateProbabilities: []float64{1},
		ClassOrder:            []string{"provider-aware"},
		ClassProbabilities:    map[string]float64{"provider-aware": 1},
		RankedFallback: []policy.PreviewGroup{{
			Group:        "provider-aware",
			Probability:  1,
			EligibleArms: []string{selectedArmID},
		}},
		SelectedGroup:     "provider-aware",
		EligibleRosterIDs: []string{selectedArmID},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.Strategy("temporal-q"),
	}, decider, resolver).WithCapabilities(policy.Capabilities{SupportsPreview: true})

	result, err := adapter.PreviewRoute(context.Background(), router.Request{})

	require.NoError(t, err)
	assert.Equal(t, policy.SchemaVersionV2, decider.previewQuery.SchemaVersion)
	assert.Equal(t, []string{selectedArmID}, result.EligibleRosterIDs)
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

// clusterOverrideMapper mirrors the HMM roster aliasing closely enough for the
// override path: anthropic models map to "anthropic/<id>".
func clusterOverrideMapper(model catalog.Model) string { return "anthropic/" + model.ID }

func TestSidecarRouterAppliesClusterArmOverride(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "claude-sonnet-5"),
		set(providers.ProviderAnthropic),
		clusterOverrideMapper,
		policy.ManagedProviderPolicy(),
	)
	// Sidecar classifies into "maximum" and would serve opus first.
	decider := &recordingPolicy{result: policy.Result{
		SchemaVersion: policy.SchemaVersionV1,
		Model:         "anthropic/claude-opus-4-8",
		Provider:      providers.ProviderAnthropic,
		Score:         0.8,
		PolicyGroup:   "maximum",
		RankedFallback: []policy.PreviewGroup{{
			Group:        "maximum",
			Probability:  0.8,
			RosterArms:   []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
			EligibleArms: []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
		}},
	}}
	// Capability flag is deliberately false: the presence of ranked_fallback in
	// the /route response must be enough to enforce overrides, so a stale
	// boot-time capability (older sidecar, no refresh) can't block them.
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, resolver).WithCapabilities(policy.Capabilities{
		SchemaVersion:         policy.SchemaVersionV1,
		ReportsRankedFallback: false,
	})

	// The key reorders the maximum cluster so sonnet-5 wins over opus.
	decision, err := adapter.Route(context.Background(), router.Request{
		ClusterArmOverrides: map[string][]string{
			"maximum": {"claude-sonnet-5", "claude-opus-4-8"},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "claude-sonnet-5", decision.Model,
		"override order must decide the served model, not the sidecar's first arm")
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
	assert.Contains(t, decision.Reason, "cluster_override",
		"an override that changes the pick must annotate the reason")
}

func TestSidecarRouterClusterOverrideResolvesArmEnumeratingBinding(t *testing.T) {
	// Arm-enumerating resolver: arm IDs differ from roster IDs. The override
	// selects a roster ID, so binding resolution must go through ByRosterID —
	// passing the roster ID as an arm ID would miss ByArmID and hard-fail.
	resolver := policy.NewArmResolver(
		set("claude-opus-4-8", "claude-sonnet-5"),
		set(providers.ProviderAnthropic),
		clusterOverrideMapper,
		policy.ManagedProviderPolicy(),
	)
	resolved := resolver.Resolve(router.Request{})
	require.NotEmpty(t, resolved.Candidates)
	// Confirm the premise: arm IDs are not equal to roster IDs on this resolver.
	require.NotEqual(t, resolved.Candidates[0].ArmID, resolved.Candidates[0].RosterID)

	decider := &recordingPolicy{result: policy.Result{
		SchemaVersion: policy.SchemaVersionV2,
		ArmID:         resolved.Candidates[0].ArmID,
		Model:         resolved.Candidates[0].RosterID,
		Provider:      providers.ProviderAnthropic,
		Score:         0.8,
		RankedFallback: []policy.PreviewGroup{{
			Group:        "maximum",
			Probability:  0.8,
			RosterArms:   []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
			EligibleArms: []string{"anthropic/claude-opus-4-8", "anthropic/claude-sonnet-5"},
		}},
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, resolver)

	decision, err := adapter.Route(context.Background(), router.Request{
		ClusterArmOverrides: map[string][]string{
			"maximum": {"claude-sonnet-5", "claude-opus-4-8"},
		},
	})

	require.NoError(t, err, "override must resolve via ByRosterID on an arm-enumerating resolver")
	assert.Equal(t, "claude-sonnet-5", decision.Model)
	assert.Equal(t, providers.ProviderAnthropic, decision.Provider)
}

func TestSidecarRouterClusterOverrideFailsOpenWithoutRankedFallback(t *testing.T) {
	resolver := policy.NewResolver(
		set("claude-opus-4-8", "claude-sonnet-5"),
		set(providers.ProviderAnthropic),
		clusterOverrideMapper,
		policy.ManagedProviderPolicy(),
	)
	// Old sidecar: no ranked fallback in the result, capability off.
	decider := &recordingPolicy{result: policy.Result{
		SchemaVersion: policy.SchemaVersionV1,
		Model:         "anthropic/claude-opus-4-8",
		Provider:      providers.ProviderAnthropic,
		Score:         0.8,
	}}
	adapter := policy.NewSidecarRouter(policy.SidecarRouterConfig{
		Strategy: router.StrategyHMM,
	}, decider, resolver).WithCapabilities(policy.Capabilities{
		SchemaVersion:         policy.SchemaVersionV1,
		ReportsRankedFallback: false,
	})

	decision, err := adapter.Route(context.Background(), router.Request{
		ClusterArmOverrides: map[string][]string{
			"maximum": {"claude-sonnet-5"},
		},
	})

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", decision.Model,
		"an old sidecar without ranked fallback must serve its own selection unchanged")
	assert.NotContains(t, decision.Reason, "cluster_override")
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
