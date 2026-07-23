package auth

import (
	"context"
	"time"
)

type APIKey struct {
	ID             string
	InstallationID string
	ExternalID     string
	Name           *string
	KeyPrefix      string
	KeyHash        string
	KeySuffix      string
	LastUsedAt     *time.Time
	CreatedAt      time.Time
	DeletedAt      *time.Time
	CreatedBy      *string
}

type CreateAPIKeyParams struct {
	InstallationID string
	ExternalID     string
	Name           *string
	KeyPrefix      string
	KeyHash        string
	KeySuffix      string
	CreatedBy      *string
}

type APIKeyRepository interface {
	Create(ctx context.Context, params CreateAPIKeyParams) (*APIKey, error)
	GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*APIKey, *Installation, error)
	ListForInstallation(ctx context.Context, installationID string) ([]*APIKey, error)
	MarkUsed(ctx context.Context, id string) error
	// SoftDelete soft-deletes the key when it is still active and returns the
	// rows-affected count, letting a caller distinguish a real transition from
	// a no-op.
	SoftDelete(ctx context.Context, installationID, id string) (int64, error)
}
