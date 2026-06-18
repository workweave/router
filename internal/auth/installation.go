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
	// RoutingQualityWeight is the per-installation routing preference (the
	// "quality vs price" dial), stored as the scorer's quality weight (Alpha)
	// -- a normalized fraction in [0, 1] where 1.0 biases routing fully toward
	// quality and 0.0 fully toward price. The implied price weight is the
	// remainder. nil means no preference -- the scorer keeps its tuned
	// per-cluster defaults.
	RoutingQualityWeight *float64
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
}
