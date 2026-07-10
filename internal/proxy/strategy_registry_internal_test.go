package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"
)

type registryRouter struct {
	decision router.Decision
	calls    int
}

func (r *registryRouter) Route(context.Context, router.Request) (router.Decision, error) {
	r.calls++
	return r.decision, nil
}

func TestPolicyStrategyRegistryRoutesFutureStrategyWithoutServiceChanges(t *testing.T) {
	defaultRouter := &registryRouter{}
	futureRouter := &registryRouter{decision: router.Decision{Model: "claude-opus-4-8", Provider: providers.ProviderAnthropic}}
	strategy := router.Strategy("future-policy")
	svc := (&Service{router: defaultRouter}).WithPolicyStrategy(policy.StrategySpec{
		Strategy: strategy,
		Router:   futureRouter,
		Capabilities: policy.Capabilities{
			SchemaVersion:   policy.SchemaVersionV1,
			ReportsFeedback: true,
		},
	})

	decision, err := svc.Route(router.WithStrategy(context.Background(), strategy), router.Request{})

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", decision.Model)
	assert.Equal(t, 1, futureRouter.calls)
	assert.Zero(t, defaultRouter.calls)
	capabilities, ok := svc.PolicyCapabilities(strategy)
	require.True(t, ok)
	assert.True(t, capabilities.ReportsFeedback)
}

func TestPolicyStrategyRegistryFailsClosedForUnknownStrategy(t *testing.T) {
	defaultRouter := &registryRouter{}
	svc := &Service{router: defaultRouter}

	_, err := svc.Route(router.WithStrategy(context.Background(), router.Strategy("missing-policy")), router.Request{})

	require.Error(t, err)
	assert.ErrorIs(t, err, router.ErrStrategyUnavailable)
	assert.Zero(t, defaultRouter.calls)
}
