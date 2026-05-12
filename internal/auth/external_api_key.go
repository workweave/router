package auth

import (
	"context"
	"time"
)

// ExternalAPIKey represents a customer-owned provider API key.
type ExternalAPIKey struct {
	ID             string
	InstallationID string
	Provider       string // "anthropic" | "openai" | "google"
	Name           *string
	KeyPrefix      string
	KeySuffix      string
	KeyFingerprint string
	CreatedAt      time.Time
	LastUsedAt     *time.Time
	// Plaintext is populated after decrypt; never logged.
	Plaintext []byte
}

type CreateExternalAPIKeyParams struct {
	InstallationID string
	ExternalID     string
	Provider       string
	KeyCiphertext  []byte
	KeyPrefix      string
	KeySuffix      string
	KeyFingerprint string
	Name           *string
	CreatedBy      *string
}

// ExternalAPIKeyRepository manages external API keys in storage.
type ExternalAPIKeyRepository interface {
	// Create inserts a new external API key. Callers must soft-delete any existing key for the same (installation, provider) first.
	Create(ctx context.Context, params CreateExternalAPIKeyParams) (*ExternalAPIKey, error)
	// GetForInstallation returns all active keys with Plaintext populated after decryption.
	GetForInstallation(ctx context.Context, installationID string) ([]*ExternalAPIKey, error)
	SoftDeleteByProvider(ctx context.Context, installationID, provider string) error
	SoftDelete(ctx context.Context, installationID, id string) error
	// MarkUsed updates last_used_at. Fire-and-forget, non-blocking.
	MarkUsed(ctx context.Context, id string) error
}
