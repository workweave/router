package auth

import (
	"context"
	"time"
)

// UserIDContextKey is the request-context key for the resolved router user ID.
type UserIDContextKey struct{}

// UserIDFrom returns the router user ID stashed on ctx, or "" if absent.
func UserIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(UserIDContextKey{}).(string)
	return s
}

// User is an end-user identity scoped to an installation. The API key authenticates the installation;
// User identifies which seat made the request (from git user.email in metadata.user_id or X-Weave-User-Email).
// Email may be empty when only account_uuid is available (Claude CLI versions that pack only account_uuid).
type User struct {
	ID                string
	InstallationID    string
	Email             string // "" when the row is account_uuid-only
	ClaudeAccountUUID *string
	FirstSeenAt       time.Time
	LastSeenAt        time.Time
	DeletedAt         *time.Time
}

// UpsertUserParams is the input to UserRepository.UpsertByEmail. Email must be normalized
// (lowercased+trimmed) by the caller — the DB unique index is case-sensitive.
type UpsertUserParams struct {
	InstallationID    string
	Email             string
	ClaudeAccountUUID *string
}

// UpsertUserByAccountUUIDParams is the input to UserRepository.UpsertByAccountUUID.
// Used when the inbound request carries a Claude account_uuid but no email.
type UpsertUserByAccountUUIDParams struct {
	InstallationID    string
	ClaudeAccountUUID string
}

// UserRepository is the data-access port for end-user identities.
type UserRepository interface {
	// UpsertByEmail finds-or-creates a row keyed on (installation_id, email), refreshing last_seen_at
	// and merging a non-empty claude_account_uuid. Idempotent.
	UpsertByEmail(ctx context.Context, params UpsertUserParams) (*User, error)
	// UpsertByAccountUUID finds-or-creates an email-NULL row keyed on (installation_id, claude_account_uuid).
	// Idempotent. Used when the inbound request has no email signal.
	UpsertByAccountUUID(ctx context.Context, params UpsertUserByAccountUUIDParams) (*User, error)
	Get(ctx context.Context, id string) (*User, error)
	ListForInstallation(ctx context.Context, installationID string) ([]*User, error)
}
