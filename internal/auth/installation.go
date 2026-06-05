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
	// RoutingQualityWeight and RoutingSpeedWeight are the per-installation
	// routing preference ("speed / price / quality" dials), stored as
	// normalized fractions in [0, 1]. Quality overrides the scorer's
	// per-cluster Alpha and speed maps to SpeedWeight; price is the implied
	// remainder. nil means no preference -- the scorer keeps its tuned
	// per-cluster defaults. The two are written and cleared as a pair.
	RoutingQualityWeight *float64
	RoutingSpeedWeight   *float64
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
}
