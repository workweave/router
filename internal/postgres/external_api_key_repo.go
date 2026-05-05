package postgres

import (
	"context"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
)

// ExternalAPIKeyRepo implements auth.ExternalAPIKeyRepository over SQLC.
type ExternalAPIKeyRepo struct {
	tx        sqlc.DBTX
	encryptor auth.Encryptor
}

// NewExternalAPIKeyRepo constructs an ExternalAPIKeyRepo.
func NewExternalAPIKeyRepo(tx sqlc.DBTX, encryptor auth.Encryptor) *ExternalAPIKeyRepo {
	return &ExternalAPIKeyRepo{tx: tx, encryptor: encryptor}
}

// Create inserts a new external API key.
func (r *ExternalAPIKeyRepo) Create(ctx context.Context, params auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	installationUUID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}

	q := sqlc.New(r.tx)
	row, err := q.CreateExternalAPIKey(ctx, sqlc.CreateExternalAPIKeyParams{
		InstallationID: installationUUID,
		ExternalID:     params.ExternalID,
		Provider:       params.Provider,
		KeyCiphertext:  params.KeyCiphertext,
		KeyPrefix:      params.KeyPrefix,
		KeySuffix:      params.KeySuffix,
		KeyFingerprint: params.KeyFingerprint,
		Name:           params.Name,
		CreatedBy:      params.CreatedBy,
	})
	if err != nil {
		return nil, err
	}

	return toExternalAPIKey(row), nil
}

// GetForInstallation returns all active keys for an installation with Plaintext populated.
func (r *ExternalAPIKeyRepo) GetForInstallation(ctx context.Context, installationID string) ([]*auth.ExternalAPIKey, error) {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}

	q := sqlc.New(r.tx)
	rows, err := q.GetActiveExternalAPIKeysForInstallation(ctx, installationUUID)
	if err != nil {
		return nil, err
	}

	keys := make([]*auth.ExternalAPIKey, 0, len(rows))
	for _, row := range rows {
		key := toExternalAPIKey(row)
		plaintext, err := r.encryptor.Decrypt(row.KeyCiphertext)
		if err != nil {
			return nil, err
		}
		key.Plaintext = plaintext
		keys = append(keys, key)
	}
	return keys, nil
}

// SoftDeleteByProvider soft-deletes the existing key for a provider.
func (r *ExternalAPIKeyRepo) SoftDeleteByProvider(ctx context.Context, installationID, provider string) error {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.SoftDeleteExternalAPIKeyByProvider(ctx, sqlc.SoftDeleteExternalAPIKeyByProviderParams{
		InstallationID: installationUUID,
		Provider:       provider,
	})
}

// SoftDelete soft-deletes a specific key by ID.
func (r *ExternalAPIKeyRepo) SoftDelete(ctx context.Context, installationID, id string) error {
	installationUUID, err := uuid.Parse(installationID)
	if err != nil {
		return err
	}
	keyUUID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.SoftDeleteExternalAPIKey(ctx, sqlc.SoftDeleteExternalAPIKeyParams{
		ID:             keyUUID,
		InstallationID: installationUUID,
	})
}

// MarkUsed updates last_used_at for the given key.
func (r *ExternalAPIKeyRepo) MarkUsed(ctx context.Context, id string) error {
	keyUUID, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.MarkExternalAPIKeyUsed(ctx, keyUUID)
}

func toExternalAPIKey(row sqlc.RouterModelRouterExternalAPIKey) *auth.ExternalAPIKey {
	key := &auth.ExternalAPIKey{
		ID:             row.ID.String(),
		InstallationID: row.InstallationID.String(),
		Provider:       row.Provider,
		KeyPrefix:      row.KeyPrefix,
		KeySuffix:      row.KeySuffix,
		KeyFingerprint: row.KeyFingerprint,
		CreatedAt:      timestampOrZero(row.CreatedAt),
	}
	if row.Name != nil {
		key.Name = row.Name
	}
	key.LastUsedAt = timestampPtr(row.LastUsedAt)
	return key
}
