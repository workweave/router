package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/router/policy"
)

type captureHMMOutcomeReporter struct {
	ch chan map[string]interface{}
}

func (r *captureHMMOutcomeReporter) ReportOutcome(ctx context.Context, payload map[string]interface{}) error {
	select {
	case r.ch <- payload:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *captureHMMOutcomeReporter) Route(context.Context, router.Request) (router.Decision, error) {
	return router.Decision{}, nil
}

func TestReportPolicyOutcome_UsesFreshMetadataForStickyServedDecision(t *testing.T) {
	reporter := &captureHMMOutcomeReporter{ch: make(chan map[string]interface{}, 1)}
	s := (&Service{}).WithPolicyStrategy(policy.StrategySpec{Strategy: router.StrategyHMM, Router: reporter})

	routeRes := turnLoopResult{
		StickyHit: true,
		Fresh: router.Decision{
			Model:    "moonshotai/kimi-k2.7",
			Provider: providers.ProviderFireworks,
			Metadata: &router.RoutingMetadata{
				RouteID:          "route-fresh",
				Strategy:         string(router.StrategyHMM),
				PolicyRouteKey:   "medium|mid",
				PolicyArtifactID: "hmm-prod",
			},
		},
	}
	served := router.Decision{
		Model:    "claude-haiku-4-5",
		Provider: providers.ProviderAnthropic,
	}

	ctx := context.WithValue(context.Background(), PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, ExternalIDContextKey{}, "org-1")
	ctx = context.WithValue(ctx, InstallationIDContextKey{}, "installation-1")
	ctx = context.WithValue(ctx, ClientIdentityContextKey{}, ClientIdentity{ClientApp: ClientAppCodex, RolloutID: "rollout-1"})
	const (
		inputTokens  = 90
		outputTokens = 10
	)
	s.reportPolicyOutcome(ctx, routeRes, served, providers.ProviderAnthropic, 100, inputTokens, outputTokens, 0, 0, 12, 34, nil, &policyOutcomeResponse{
		Body:      []byte(`{"content":[{"type":"text","text":"done"}]}`),
		Truncated: false,
	})

	price, ok := catalog.PriceFor(providers.ProviderAnthropic, "claude-haiku-4-5")
	require.True(t, ok)
	wantCost := catalog.EffectiveInputCost(inputTokens, 0, 0, price.InputUSDPer1M, price, providers.ProviderAnthropic) +
		catalog.EffectiveOutputCost(outputTokens, price.OutputUSDPer1M)

	select {
	case payload := <-reporter.ch:
		require.Equal(t, "route-fresh", payload["route_id"])
		assert.Equal(t, "claude-haiku-4-5", payload["served_model"])
		assert.Equal(t, providers.ProviderAnthropic, payload["served_provider"])
		assert.Equal(t, "moonshotai/kimi-k2.7", payload["decision_model"])
		assert.Equal(t, providers.ProviderFireworks, payload["decision_provider"])
		assert.Equal(t, "medium|mid", payload["policy_route_key"])
		assert.Equal(t, "hmm-prod", payload["policy_artifact_id"])
		assert.Equal(t, "org-1", payload["organization_id"])
		assert.Equal(t, "installation-1", payload["installation_id"])
		assert.Equal(t, ClientAppCodex, payload["client_app"])
		assert.Equal(t, "rollout-1", payload["rollout_id"])
		assert.Equal(t, true, payload["training_allowed"])
		assert.Equal(t, true, payload["sticky_hit"])
		assert.Equal(t, `{"content":[{"type":"text","text":"done"}]}`, payload["response_body"])
		assert.Equal(t, "client_anthropic", payload["response_body_format"])
		assert.Equal(t, false, payload["response_body_truncated"])
		assert.Equal(t, wantCost, payload["cost_usd"])
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HMM outcome payload")
	}
}

func TestReportPolicyOutcome_OmitsResponseBodyWhenTrainingIsNotAllowed(t *testing.T) {
	reporter := &captureHMMOutcomeReporter{ch: make(chan map[string]interface{}, 1)}
	s := (&Service{}).WithPolicyStrategy(policy.StrategySpec{Strategy: router.StrategyHMM, Router: reporter})
	routeRes := turnLoopResult{Fresh: router.Decision{
		Model:    "moonshotai/kimi-k2.7",
		Metadata: &router.RoutingMetadata{RouteID: "route-1", Strategy: string(router.StrategyHMM)},
	}}

	s.reportPolicyOutcome(context.Background(), routeRes, routeRes.Fresh, providers.ProviderFireworks, 1, 1, 1, 0, 0, 1, 1, nil, &policyOutcomeResponse{Body: []byte("private response")})

	select {
	case payload := <-reporter.ch:
		assert.Equal(t, false, payload["training_allowed"])
		assert.NotContains(t, payload, "response_body")
		assert.NotContains(t, payload, "response_body_truncated")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for policy outcome payload")
	}
}
