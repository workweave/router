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
	request  router.Request
}

func (r *registryRouter) Route(_ context.Context, request router.Request) (router.Decision, error) {
	r.calls++
	r.request = request
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

	ctx := context.WithValue(context.Background(), ExternalIDContextKey{}, "org-1")
	ctx = context.WithValue(ctx, InstallationIDContextKey{}, "11111111-1111-1111-1111-111111111111")
	ctx = context.WithValue(ctx, ClientIdentityContextKey{}, ClientIdentity{ClientApp: ClientAppCodex, RolloutID: "rollout-1"})
	ctx = context.WithValue(ctx, PolicyTrainingAllowedContextKey{}, true)
	ctx = context.WithValue(ctx, PolicyDebugEnabledContextKey{}, true)
	ctx = context.WithValue(ctx, PolicyRoutingIntentContextKey{}, "high")
	ctx = router.WithStrategy(ctx, strategy)
	svc.captureMode = CaptureFull

	decision, err := svc.Route(ctx, router.Request{})

	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-8", decision.Model)
	assert.Equal(t, 1, futureRouter.calls)
	assert.Zero(t, defaultRouter.calls)
	assert.Equal(t, "org-1", futureRouter.request.OrganizationID)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", futureRouter.request.InstallationID)
	assert.Equal(t, ClientAppCodex, futureRouter.request.ClientApp)
	assert.Equal(t, "rollout-1", futureRouter.request.RolloutID)
	assert.Equal(t, "full", futureRouter.request.CaptureMode)
	assert.True(t, futureRouter.request.TrainingAllowed)
	assert.True(t, futureRouter.request.DebugEnabled)
	assert.Equal(t, "high", futureRouter.request.RoutingIntent)
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
