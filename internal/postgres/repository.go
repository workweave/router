// Package postgres implements auth.InstallationRepository and
// auth.APIKeyRepository over the SQLC-generated *sqlc.Queries.
package postgres

import (
	"context"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
)

type Repository struct {
	Installations auth.InstallationRepository
	APIKeys       auth.APIKeyRepository
}

func NewRepository(tx sqlc.DBTX) *Repository {
	return &Repository{
		Installations: &installationRepo{tx: tx},
		APIKeys:       &apiKeyRepo{tx: tx},
	}
}

type installationRepo struct {
	tx sqlc.DBTX
}

func (r *installationRepo) Create(ctx context.Context, params auth.CreateInstallationParams) (*auth.Installation, error) {
	q := sqlc.New(r.tx)
	row, err := q.CreateModelRouterInstallation(ctx, sqlc.CreateModelRouterInstallationParams{
		ExternalID:        params.ExternalID,
		Name:              params.Name,
		CreatedBy:         params.CreatedBy,
		IsEvalAllowlisted: params.IsEvalAllowlisted,
	})
	if err != nil {
		return nil, err
	}
	return toAuthInstallation(row), nil
}

func (r *installationRepo) SetEvalAllowlisted(ctx context.Context, externalID, id string, allowlisted bool) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.SetModelRouterInstallationEvalAllowlisted(ctx, sqlc.SetModelRouterInstallationEvalAllowlistedParams{
		ID:                parsed,
		ExternalID:        externalID,
		IsEvalAllowlisted: allowlisted,
	})
}

func (r *installationRepo) Get(ctx context.Context, externalID, id string) (*auth.Installation, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	row, err := q.GetModelRouterInstallation(ctx, sqlc.GetModelRouterInstallationParams{
		ID:         parsed,
		ExternalID: externalID,
	})
	if err != nil {
		return nil, err
	}
	return toAuthInstallation(row), nil
}

func (r *installationRepo) ListForExternalID(ctx context.Context, externalID string) ([]*auth.Installation, error) {
	q := sqlc.New(r.tx)
	rows, err := q.ListModelRouterInstallationsForExternalID(ctx, externalID)
	if err != nil {
		return nil, err
	}
	out := make([]*auth.Installation, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuthInstallation(row))
	}
	return out, nil
}

func (r *installationRepo) SoftDelete(ctx context.Context, externalID, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.SoftDeleteModelRouterInstallation(ctx, sqlc.SoftDeleteModelRouterInstallationParams{
		ID:         parsed,
		ExternalID: externalID,
	})
}

type apiKeyRepo struct {
	tx sqlc.DBTX
}

func (r *apiKeyRepo) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	installationID, err := uuid.Parse(params.InstallationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	row, err := q.CreateModelRouterAPIKey(ctx, sqlc.CreateModelRouterAPIKeyParams{
		InstallationID: installationID,
		ExternalID:     params.ExternalID,
		Name:           params.Name,
		KeyPrefix:      params.KeyPrefix,
		KeyHash:        params.KeyHash,
		KeySuffix:      params.KeySuffix,
		CreatedBy:      params.CreatedBy,
	})
	if err != nil {
		return nil, err
	}
	return toAuthAPIKey(row), nil
}

func (r *apiKeyRepo) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	q := sqlc.New(r.tx)
	row, err := q.GetActiveModelRouterAPIKeyWithInstallationByHash(ctx, keyHash)
	if err != nil {
		return nil, nil, err
	}
	return toAuthAPIKey(row.RouterModelRouterAPIKey), toAuthInstallation(row.RouterModelRouterInstallation), nil
}

func (r *apiKeyRepo) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	parsed, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.ListModelRouterAPIKeysForInstallation(ctx, parsed)
	if err != nil {
		return nil, err
	}
	out := make([]*auth.APIKey, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAuthAPIKey(row))
	}
	return out, nil
}

func (r *apiKeyRepo) MarkUsed(ctx context.Context, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.MarkModelRouterAPIKeyUsed(ctx, parsed)
}

func (r *apiKeyRepo) SoftDelete(ctx context.Context, id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.SoftDeleteModelRouterAPIKey(ctx, parsed)
}
