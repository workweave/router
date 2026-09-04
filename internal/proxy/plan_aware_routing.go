package proxy

import (
	"context"
	"net/http"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/flags"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/subscriptions"
)

// SubscriptionPlanState identifies the aggregate availability of one
// provider subscription family for the authenticated router user.
type SubscriptionPlanState string

const (
	// SubscriptionPlanStateActive means at least one account has usable
	// subscription headroom.
	SubscriptionPlanStateActive SubscriptionPlanState = "active"
	// SubscriptionPlanStateExhausted means every enabled account is cooling
	// down after a quota response.
	SubscriptionPlanStateExhausted SubscriptionPlanState = "exhausted"
	// SubscriptionPlanStateUnknown means the account exists but its state
	// cannot be safely interpreted as quota exhaustion.
	SubscriptionPlanStateUnknown SubscriptionPlanState = "unknown"
)

// ManagedSubscriptionPlanStatesContextKey carries aggregate managed account
// states populated by authentication middleware.
type ManagedSubscriptionPlanStatesContextKey struct{}

// SubscriptionPlanAwareExcludedModelsContextKey carries models removed from
// automatic routing for this request by plan-aware routing.
type SubscriptionPlanAwareExcludedModelsContextKey struct{}

// ManagedSubscriptionPlanStates derives provider-level state from durable
// managed account metadata. Disabled accounts are unknown rather than
// exhausted because disabled commonly means authentication or operator state,
// not quota exhaustion.
func ManagedSubscriptionPlanStates(accounts []*auth.SubscriptionAccount, now time.Time) map[subscriptions.Provider]SubscriptionPlanState {
	states := make(map[subscriptions.Provider]SubscriptionPlanState)
	enabledAccounts := make(map[subscriptions.Provider]int)
	activeAccounts := make(map[subscriptions.Provider]int)

	for _, account := range accounts {
		if account == nil {
			continue
		}
		provider := subscriptions.Provider(account.Provider)
		if provider != subscriptions.ProviderClaude && provider != subscriptions.ProviderCodex {
			continue
		}
		if !account.Enabled {
			if _, exists := states[provider]; !exists {
				states[provider] = SubscriptionPlanStateUnknown
			}
			continue
		}
		enabledAccounts[provider]++
		if account.CooldownUntil == nil || !account.CooldownUntil.After(now) {
			activeAccounts[provider]++
		}
	}

	for provider, count := range enabledAccounts {
		switch {
		case activeAccounts[provider] > 0:
			states[provider] = SubscriptionPlanStateActive
		case count > 0:
			states[provider] = SubscriptionPlanStateExhausted
		}
	}
	return states
}

func managedSubscriptionPlanStatesFromContext(ctx context.Context) map[subscriptions.Provider]SubscriptionPlanState {
	states, _ := ctx.Value(ManagedSubscriptionPlanStatesContextKey{}).(map[subscriptions.Provider]SubscriptionPlanState)
	return states
}

func subscriptionPlanAwareExcludedModelsFromContext(ctx context.Context) map[string]struct{} {
	excluded, _ := ctx.Value(SubscriptionPlanAwareExcludedModelsContextKey{}).(map[string]struct{})
	return excluded
}

func subscriptionPlanStatesForRequest(s *Service, ctx context.Context, headers http.Header) map[subscriptions.Provider]SubscriptionPlanState {
	states := make(map[subscriptions.Provider]SubscriptionPlanState, 2)
	managedStates := managedSubscriptionPlanStatesFromContext(ctx)
	for provider, state := range managedStates {
		states[provider] = state
	}

	codexToken, claudeToken := presentSubscriptionTokens(ctx, headers)
	directTokens := map[subscriptions.Provider]string{
		subscriptions.ProviderClaude: claudeToken,
		subscriptions.ProviderCodex:  codexToken,
	}
	for provider, token := range directTokens {
		if token == "" {
			continue
		}
		state := SubscriptionPlanStateActive
		if s.usageObserver != nil {
			snapshot, observed := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(token)))
			if observed && snapshot.Exhausted() {
				state = SubscriptionPlanStateExhausted
			}
		}
		managedState, managedConfigured := states[provider]
		switch {
		case !managedConfigured:
			states[provider] = state
		case managedState == SubscriptionPlanStateActive || state == SubscriptionPlanStateActive:
			states[provider] = SubscriptionPlanStateActive
		case managedState == SubscriptionPlanStateUnknown || state == SubscriptionPlanStateUnknown:
			states[provider] = SubscriptionPlanStateUnknown
		default:
			states[provider] = SubscriptionPlanStateExhausted
		}
	}
	return states
}

func subscriptionPlanCoversModel(provider subscriptions.Provider, model string) bool {
	switch provider {
	case subscriptions.ProviderClaude:
		if catalogModel, ok := catalog.ByID(model); ok {
			return catalogModel.PrimaryProvider() == providers.ProviderAnthropic
		}
	case subscriptions.ProviderCodex:
		return codexSubscriptionCoversModel(model)
	}
	return false
}

func planAwareExcludedModels(states map[subscriptions.Provider]SubscriptionPlanState, universe map[string]struct{}) map[string]struct{} {
	if len(states) == 0 {
		return nil
	}

	activePlans := 0
	for _, state := range states {
		switch state {
		case SubscriptionPlanStateActive:
			activePlans++
		case SubscriptionPlanStateUnknown:
			return nil
		case SubscriptionPlanStateExhausted:
		default:
			return nil
		}
	}
	if allSubscriptionPlansExhausted(states) {
		return nil
	}
	if activePlans == 0 {
		return nil
	}

	excluded := make(map[string]struct{})
	for model := range universe {
		coveredByActivePlan := false
		coveredByExhaustedPlan := false
		for provider, state := range states {
			if !subscriptionPlanCoversModel(provider, model) {
				continue
			}
			switch state {
			case SubscriptionPlanStateActive:
				coveredByActivePlan = true
			case SubscriptionPlanStateExhausted:
				coveredByExhaustedPlan = true
			}
		}
		if coveredByExhaustedPlan && !coveredByActivePlan {
			excluded[model] = struct{}{}
		}
	}
	if len(excluded) == 0 {
		return nil
	}
	return excluded
}

func allSubscriptionPlansExhausted(states map[subscriptions.Provider]SubscriptionPlanState) bool {
	if len(states) == 0 {
		return false
	}
	for _, state := range states {
		if state != SubscriptionPlanStateExhausted {
			return false
		}
	}
	return true
}

func managedSubscriptionPlansAllExhausted(ctx context.Context) bool {
	return allSubscriptionPlansExhausted(managedSubscriptionPlanStatesFromContext(ctx))
}

func subscriptionPlanAwareRoutingEnabled(ctx context.Context) bool {
	return !subscriptionRoutingDisabledForRequest(ctx) && flags.BoolOr(ctx, flags.KeySubscriptionPlanAwareRouting, false)
}

func (s *Service) withPlanAwareSubscriptionModels(ctx context.Context, headers http.Header) context.Context {
	if !subscriptionPlanAwareRoutingEnabled(ctx) {
		return ctx
	}
	states := subscriptionPlanStatesForRequest(s, ctx, headers)
	excluded := planAwareExcludedModels(states, s.routableUniverse())
	if len(excluded) == 0 {
		return ctx
	}
	return context.WithValue(ctx, SubscriptionPlanAwareExcludedModelsContextKey{}, excluded)
}
