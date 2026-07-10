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
	// DefaultStrategy is the per-key routing-strategy default, used when the
	// caller can't send x-weave-router-strategy (e.g. Cursor's Override Base
	// URL, which has no custom-header field). Empty means no key default --
	// the deployment default (cluster) applies. The header always wins over
	// this value when present; see WithRouterStrategyOverride.
	DefaultStrategy string
}

type CreateAPIKeyParams struct {
	InstallationID  string
	ExternalID      string
	Name            *string
	KeyPrefix       string
	KeyHash         string
	KeySuffix       string
	DefaultStrategy string
	CreatedBy       *string
}

type APIKeyRepository interface {
	Create(ctx context.Context, params CreateAPIKeyParams) (*APIKey, error)
	GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*APIKey, *Installation, error)
	ListForInstallation(ctx context.Context, installationID string) ([]*APIKey, error)
	MarkUsed(ctx context.Context, id string) error
	SoftDelete(ctx context.Context, installationID, id string) error
	// UpdateDefaultStrategy sets the per-key default_strategy, scoped to
	// installationID for cross-tenant safety. Empty defaultStrategy clears it
	// (stored as NULL). Returns ErrAPIKeyNotFound if id isn't an active key
	// owned by installationID.
	UpdateDefaultStrategy(ctx context.Context, installationID, id, defaultStrategy string) error
}
