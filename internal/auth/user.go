package auth

import (
	"context"
	"time"
)

// UserIDContextKey is the request-context key for the resolved router user ID.
// Handlers stash the User.ID after Service.ResolveAndStashUser; downstream
// code (proxy.Service, OTEL spans, decision log) reads it via UserIDFrom.
type UserIDContextKey struct{}

// UserIDFrom returns the router user ID stashed on ctx, or "" when the request
// carried no email signal.
func UserIDFrom(ctx context.Context) string {
	s, _ := ctx.Value(UserIDContextKey{}).(string)
	return s
}

// User is an end-user identity seen on inbound requests, scoped to an installation.
// Replaces the previous "one API key per human" pattern: today the API key
// authenticates the installation, and User identifies which seat made the request
// (typically derived from git user.email carried in metadata.user_id or the
// X-Weave-User-Email header).
type User struct {
	ID                string
	InstallationID    string
	Email             string
	ClaudeAccountUUID *string
	FirstSeenAt       time.Time
	LastSeenAt        time.Time
	DeletedAt         *time.Time
}

// UpsertUserParams is the input to UserRepository.Upsert. Email must already be
// normalized (lowercased + trimmed) by the caller — the unique index is
// case-sensitive at the database layer.
type UpsertUserParams struct {
	InstallationID    string
	Email             string
	ClaudeAccountUUID *string
}

// UserRepository is the data-access port for end-user identities. The auth
// Service writes through this on every authenticated request that carries an
// email; the dashboard reads through it for listing.
type UserRepository interface {
	// Upsert finds-or-creates a row keyed on (installation_id, email), refreshing
	// last_seen_at and merging a non-empty claude_account_uuid. Idempotent.
	Upsert(ctx context.Context, params UpsertUserParams) (*User, error)
	Get(ctx context.Context, id string) (*User, error)
	ListForInstallation(ctx context.Context, installationID string) ([]*User, error)
}
