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
	// MarkUsed records that a key was used. Called fire-and-forget after auth;
	// implementations should be idempotent on retry.
	MarkUsed(ctx context.Context, id string) error
	SoftDelete(ctx context.Context, id string) error
}
