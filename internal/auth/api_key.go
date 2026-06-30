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
	// SpendCapUsdMicros is the key's lifetime spend cap in USD micros. nil =
	// uncapped. Enforced in managed mode: once SpentUsdMicros reaches the cap
	// the key is rejected.
	SpendCapUsdMicros *int64
	// SpentUsdMicros is the key's cumulative lifetime spend in USD micros,
	// bumped in the same transaction as each org debit.
	SpentUsdMicros int64
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
	SoftDelete(ctx context.Context, installationID, id string) error
}
