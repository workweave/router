// Package auth holds installation/api-key types, repository interfaces, and the
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
}

type CreateInstallationParams struct {
	ExternalID string
	Name       string
	CreatedBy  *string
}

type InstallationRepository interface {
	Create(ctx context.Context, params CreateInstallationParams) (*Installation, error)
	// Get is scoped to externalID to prevent cross-tenant access.
	Get(ctx context.Context, externalID, id string) (*Installation, error)
	ListForExternalID(ctx context.Context, externalID string) ([]*Installation, error)
	// SoftDelete is scoped to externalID.
	SoftDelete(ctx context.Context, externalID, id string) error
}
