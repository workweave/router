package postgres

import (
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
)

func toAuthInstallation(row sqlc.RouterModelRouterInstallation) *auth.Installation {
	return &auth.Installation{
		ID:         row.ID.String(),
		ExternalID: row.ExternalID,
		Name:       row.Name,
		CreatedAt:  timestampOrZero(row.CreatedAt),
		UpdatedAt:  timestampOrZero(row.UpdatedAt),
		DeletedAt:  timestampPtr(row.DeletedAt),
		CreatedBy:  row.CreatedBy,
	}
}

func toAuthAPIKey(row sqlc.RouterModelRouterAPIKey) *auth.APIKey {
	return &auth.APIKey{
		ID:             row.ID.String(),
		InstallationID: row.InstallationID.String(),
		ExternalID:     row.ExternalID,
		Name:           row.Name,
		KeyPrefix:      row.KeyPrefix,
		KeyHash:        row.KeyHash,
		KeySuffix:      row.KeySuffix,
		LastUsedAt:     timestampPtr(row.LastUsedAt),
		CreatedAt:      timestampOrZero(row.CreatedAt),
		DeletedAt:      timestampPtr(row.DeletedAt),
		CreatedBy:      row.CreatedBy,
	}
}

func timestampOrZero(t pgtype.Timestamp) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}

func timestampPtr(t pgtype.Timestamp) *time.Time {
	if !t.Valid {
		return nil
	}
	out := t.Time
	return &out
}
