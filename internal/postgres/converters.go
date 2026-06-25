package postgres

import (
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func toAuthInstallation(row sqlc.RouterModelRouterInstallation) *auth.Installation {
	excluded := row.ExcludedModels
	if excluded == nil {
		excluded = []string{}
	}
	excludedProviders := row.ExcludedProviders
	if excludedProviders == nil {
		excludedProviders = []string{}
	}
	return &auth.Installation{
		ID:                   row.ID.String(),
		ExternalID:           row.ExternalID,
		Name:                 row.Name,
		CreatedAt:            timestampOrZero(row.CreatedAt),
		UpdatedAt:            timestampOrZero(row.UpdatedAt),
		DeletedAt:            timestampPtr(row.DeletedAt),
		CreatedBy:            row.CreatedBy,
		ExcludedModels:       excluded,
		ExcludedProviders:    excludedProviders,
		RoutingQualityWeight: row.RoutingQualityWeight,
		UsageBypassEnabled:   row.UsageBypassEnabled,
		UsageBypassThreshold: row.UsageBypassThreshold,
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

// stringPtrOrNil returns nil for empty strings so SQLC's nullable columns receive NULL instead of "".
func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// uuidOrNil parses s into a pgtype.UUID, returning an invalid (NULL) value for
// empty or malformed input so SQLC's nullable uuid column receives NULL.
func uuidOrNil(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	parsed, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: parsed, Valid: true}
}

// uuidString returns the canonical string form of a pgtype.UUID, or "" when NULL.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}
