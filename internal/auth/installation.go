// Package auth provides installation/api-key types, repository interfaces, and a
// Service that authenticates incoming bearer tokens.
package auth

import (
	"context"
	"time"
)

type Installation struct {
	ID         string
	ExternalID string
	Name       string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	DeletedAt  *time.Time
	CreatedBy  *string
	// ExcludedModels is the per-installation model exclusion list.
	// Empty means no exclusion.
	ExcludedModels []string
	// ExcludedProviders is the per-installation provider exclusion list.
	// Empty means no exclusion.
	ExcludedProviders []string
	// PreferredModels is the per-installation model priority ranking, in
	// descending preference (index 0 = first preference). The scorer lifts each
	// preferred model's score by a small, rank-decaying additive bonus so a
	// preferred model wins close calls without overriding a clearly-better
	// model. Empty means no preference.
	PreferredModels []string
	// RoutingQualityWeight is the per-installation routing preference (the
	// "quality vs price" dial), stored as the scorer's quality weight (Alpha)
	// -- a normalized fraction in [0, 1] where 1.0 biases routing fully toward
	// quality and 0.0 fully toward price. The implied price weight is the
	// remainder. nil means no preference -- the scorer keeps its tuned
	// per-cluster defaults.
	RoutingQualityWeight *float64
	// UsageBypassEnabled toggles the per-installation subscription usage-bypass
	// gate. When true, requests presenting a subscription credential whose
	// observed rate-limit utilization is below UsageBypassThreshold pass
	// straight through to the requested model (no routing, no billing debit).
	// Defaults false -- strict opt-in.
	UsageBypassEnabled bool
	// UsageBypassThreshold is the [0, 1] utilization at/above which the bypass
	// gate disengages and normal routing takes over. nil means "use the
	// deployment default" so the toggle can be on before a value is chosen.
	UsageBypassThreshold *float64
	// SubscriptionRoutingDisabled turns off subscription-AWARE ROUTING for this
	// installation. When true, the scorer's subscription subsidy bonus is
	// suppressed, so routing decides purely on quality/cost/speed merits and
	// non-Claude models compete fairly instead of always losing to the
	// subsidized Claude family. It removes only the routing BIAS: a turn that
	// still routes to Claude on its own merits is dispatched on the caller's
	// subscription token exactly as before, so the prepaid billing path is
	// unchanged. Defaults false -- preserves today's behavior.
	SubscriptionRoutingDisabled bool
}

type CreateInstallationParams struct {
	ExternalID string
	Name       string
	CreatedBy  *string
}

type InstallationRepository interface {
	Create(ctx context.Context, params CreateInstallationParams) (*Installation, error)
	Get(ctx context.Context, externalID, id string) (*Installation, error)
	ListForExternalID(ctx context.Context, externalID string) ([]*Installation, error)
	SoftDelete(ctx context.Context, externalID, id string) error
	// UpdateExcludedModels replaces the per-installation exclusion list.
	// An empty (or nil) slice clears the list.
	UpdateExcludedModels(ctx context.Context, externalID, id string, models []string) error
	// UpdateExcludedProviders replaces the per-installation provider
	// exclusion list. An empty (or nil) slice clears the list.
	UpdateExcludedProviders(ctx context.Context, externalID, id string, providerNames []string) error
	// UpdateRoutingPreference sets the routing quality weight (a normalized
	// fraction in [0, 1]). Passing nil clears the preference so the scorer
	// reverts to its tuned per-cluster defaults.
	UpdateRoutingPreference(ctx context.Context, externalID, id string, qualityWeight *float64) error
	// UpdateUsageBypass sets the subscription usage-bypass gate. enabled toggles
	// the gate; threshold is the [0, 1] utilization at/above which it disengages
	// (nil = use the deployment default).
	UpdateUsageBypass(ctx context.Context, externalID, id string, enabled bool, threshold *float64) error
	// UpdateSubscriptionRoutingDisabled toggles subscription-aware routing for
	// the installation. When true, the scorer's subscription subsidy bonus is
	// suppressed so routing decides on merits.
	UpdateSubscriptionRoutingDisabled(ctx context.Context, externalID, id string, disabled bool) error
}
