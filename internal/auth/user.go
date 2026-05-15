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

// User is an end-user identity scoped to an installation.
type User struct {
	ID                string
	InstallationID    string
	Email             string // empty when account-uuid-only
	ClaudeAccountUUID *string
	FirstSeenAt       time.Time
	LastSeenAt        time.Time
	DeletedAt         *time.Time
}

// UpsertUserParams is the input to UserRepository.UpsertByEmail.
// Email must be lowercased and trimmed by the caller.
type UpsertUserParams struct {
	InstallationID    string
	Email             string
	ClaudeAccountUUID *string
}

// UpsertUserByAccountUUIDParams is the input to UserRepository.UpsertByAccountUUID.
type UpsertUserByAccountUUIDParams struct {
	InstallationID    string
	ClaudeAccountUUID string
}

// UserRepository is the data-access port for end-user identities.
type UserRepository interface {
	// UpsertByEmail finds-or-creates a row keyed on (installation_id, email).
	UpsertByEmail(ctx context.Context, params UpsertUserParams) (*User, error)
	// UpsertByAccountUUID finds-or-creates an email-NULL row keyed on (installation_id, claude_account_uuid).
	UpsertByAccountUUID(ctx context.Context, params UpsertUserByAccountUUIDParams) (*User, error)
	Get(ctx context.Context, id string) (*User, error)
	ListForInstallation(ctx context.Context, installationID string) ([]*User, error)
}
