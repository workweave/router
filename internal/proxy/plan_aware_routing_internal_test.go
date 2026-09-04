package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/subscriptions"
)

func planAwareUniverse() map[string]struct{} {
	return map[string]struct{}{
		"claude-sonnet-5":  {},
		"claude-opus-4-8":  {},
		"gpt-5.6-sol":      {},
		"gemini-3.8-flash": {},
	}
}

func planAwareStates(states map[subscriptions.Provider]SubscriptionPlanState) context.Context {
	ctx := flags.WithOverrides(context.Background(), flags.Overrides{
		Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: true},
	})
	return context.WithValue(ctx, ManagedSubscriptionPlanStatesContextKey{}, states)
}

func TestManagedSubscriptionPlanStatesAggregatesAccountCooldowns(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)

	states := ManagedSubscriptionPlanStates([]*auth.SubscriptionAccount{
		{
			Provider: auth.SubscriptionProviderClaude,
			Enabled:  true,
		},
		{
			Provider:      auth.SubscriptionProviderCodex,
			Enabled:       true,
			CooldownUntil: &resetAt,
		},
	}, now)

	assert.Equal(t, SubscriptionPlanStateActive, states[subscriptions.ProviderClaude])
	assert.Equal(t, SubscriptionPlanStateExhausted, states[subscriptions.ProviderCodex])
}

func TestPlanAwareRoutingLeavesRosterUnchangedWhenPlansAreActive(t *testing.T) {
	svc := &Service{
		availableModels: planAwareUniverse(),
	}
	ctx := svc.withPlanAwareSubscriptionModels(planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateActive,
		subscriptions.ProviderCodex:  SubscriptionPlanStateActive,
	}), nil)

	assert.Nil(t, subscriptionPlanAwareExcludedModelsFromContext(ctx))
	assert.Nil(t, svc.excludedModelsForRequest(ctx))
}

func TestPlanAwareRoutingExcludesOnlyExhaustedPlanModels(t *testing.T) {
	svc := &Service{
		availableModels: planAwareUniverse(),
	}
	ctx := svc.withPlanAwareSubscriptionModels(planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
		subscriptions.ProviderCodex:  SubscriptionPlanStateActive,
	}), nil)

	excluded := svc.excludedModelsForRequest(ctx)

	assert.Contains(t, excluded, "claude-sonnet-5")
	assert.Contains(t, excluded, "claude-opus-4-8")
	assert.NotContains(t, excluded, "gpt-5.6-sol")
	assert.NotContains(t, excluded, "gemini-3.8-flash")
}

func TestPlanAwareRoutingRestoresNormalRosterWhenAllPlansAreExhausted(t *testing.T) {
	svc := &Service{
		availableModels: planAwareUniverse(),
	}
	ctx := svc.withPlanAwareSubscriptionModels(planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
		subscriptions.ProviderCodex:  SubscriptionPlanStateExhausted,
	}), nil)

	assert.Nil(t, subscriptionPlanAwareExcludedModelsFromContext(ctx))
	assert.Nil(t, svc.excludedModelsForRequest(ctx))
}

func TestPlanAwareRoutingDoesNotFilterOnUnknownPlanState(t *testing.T) {
	svc := &Service{
		availableModels: planAwareUniverse(),
	}
	ctx := svc.withPlanAwareSubscriptionModels(planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateUnknown,
		subscriptions.ProviderCodex:  SubscriptionPlanStateActive,
	}), nil)

	require.Nil(t, subscriptionPlanAwareExcludedModelsFromContext(ctx))
	assert.Nil(t, svc.excludedModelsForRequest(ctx))
}

func TestPlanAwareRoutingComposesWithExplicitExclusions(t *testing.T) {
	svc := &Service{
		availableModels: planAwareUniverse(),
	}
	ctx := svc.withPlanAwareSubscriptionModels(planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
		subscriptions.ProviderCodex:  SubscriptionPlanStateActive,
	}), nil)
	ctx = context.WithValue(ctx, InstallationExcludedModelsContextKey{}, []string{"gpt-5.6-sol"})

	excluded := svc.excludedModelsForRequest(ctx)

	assert.Contains(t, excluded, "claude-sonnet-5")
	assert.Contains(t, excluded, "gpt-5.6-sol")
	assert.NotContains(t, excluded, "gemini-3.8-flash")
}

func TestPlanAwareRoutingRequiresOrganizationOptIn(t *testing.T) {
	// The retired deployment setting must not opt untouched organizations in.
	t.Setenv("ROUTER_SUBSCRIPTION_PLAN_AWARE_ROUTING", "true")
	svc := &Service{availableModels: planAwareUniverse()}
	for _, tc := range []struct {
		name         string
		stored       string
		disabled     bool
		wantExcluded bool
	}{
		{name: "absent", stored: `{}`},
		{name: "org A enabled", stored: `{"subscription_plan_aware_routing_enabled":true}`, wantExcluded: true},
		{name: "org B untouched", stored: `{}`},
		{name: "org A disabled", stored: `{"subscription_plan_aware_routing_enabled":false}`},
		{name: "org A re-enabled", stored: `{"subscription_plan_aware_routing_enabled":true}`, wantExcluded: true},
		{name: "org A cleared", stored: `{}`},
		{name: "subscription routing disabled", stored: `{"subscription_plan_aware_routing_enabled":true}`, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			overrides, err := flags.ParseOverrides([]byte(tc.stored))
			require.NoError(t, err)
			ctx := flags.WithOverrides(context.Background(), overrides)
			ctx = context.WithValue(ctx, InstallationSubscriptionRoutingDisabledContextKey{}, tc.disabled)
			ctx = context.WithValue(ctx, ManagedSubscriptionPlanStatesContextKey{}, map[subscriptions.Provider]SubscriptionPlanState{
				subscriptions.ProviderClaude: SubscriptionPlanStateExhausted,
				subscriptions.ProviderCodex:  SubscriptionPlanStateActive,
			})
			ctx = svc.withPlanAwareSubscriptionModels(ctx, nil)
			excluded := svc.excludedModelsForRequest(ctx)
			if tc.wantExcluded {
				assert.Contains(t, excluded, "claude-sonnet-5")
			} else {
				assert.Empty(t, excluded)
			}
			assert.NotContains(t, excluded, "gpt-5.6-sol")
		})
	}
}

func TestPlanAwareRoutingOrgFlagAppliesToDirectSubscription(t *testing.T) {
	const token = "eyJhbGciOi.codex.jwt"
	observer := usage.NewObserver([]byte("test-salt"), time.Minute, time.Now)
	observer.Record(observer.Key([]byte(token)), usage.Snapshot{
		Primary: usage.Window{UsedPercent: 1, WindowMinutes: 300},
	})
	svc := (&Service{availableModels: planAwareUniverse()}).WithUsageObserver(observer)
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+token)
	headers.Set("ChatGPT-Account-ID", "test-account")
	ctx := planAwareStates(map[subscriptions.Provider]SubscriptionPlanState{
		subscriptions.ProviderClaude: SubscriptionPlanStateActive,
	})

	enabled := svc.withPlanAwareSubscriptionModels(ctx, headers)
	assert.Contains(t, svc.excludedModelsForRequest(enabled), "gpt-5.6-sol")
	assert.NotContains(t, svc.excludedModelsForRequest(enabled), "claude-sonnet-5")

	disabled := flags.WithOverrides(ctx, flags.Overrides{Bools: map[flags.Key]bool{flags.KeySubscriptionPlanAwareRouting: false}})
	disabled = svc.withPlanAwareSubscriptionModels(disabled, headers)
	assert.Empty(t, svc.excludedModelsForRequest(disabled))
}
