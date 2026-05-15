package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
)

// ClientIdentity holds per-request user identification signals. Email is the
// cross-protocol identity persisted to router.model_router_users.email.
type ClientIdentity struct {
	DeviceID  string
	AccountID string
	SessionID string
	Email     string
	UserAgent string
	ClientApp string
}

// ClientIdentityContextKey is the request-context key for client identity.
type ClientIdentityContextKey struct{}

// ClientIdentityFrom reads the ClientIdentity stashed on ctx.
func ClientIdentityFrom(ctx context.Context) ClientIdentity {
	id, _ := ctx.Value(ClientIdentityContextKey{}).(ClientIdentity)
	return id
}

// ResolveUserFromContext pulls identity signals from ctx and hands them to
// auth.Service.ResolveAndStashUser. No-op when deps are missing or both
// email and account_uuid are empty. Claude CLI v2.1.x packs only
// account_uuid (no email), so guarding on email alone would defeat that path.
func ResolveUserFromContext(ctx context.Context, authSvc *auth.Service, installation *auth.Installation) context.Context {
	log := observability.Get()
	if authSvc == nil || installation == nil {
		log.Info("ResolveUserFromContext bailout",
			"reason", "nil_dep",
			"authsvc_nil", authSvc == nil,
			"installation_nil", installation == nil,
		)
		return ctx
	}
	id := ClientIdentityFrom(ctx)
	if id.Email == "" && id.AccountID == "" {
		log.Info("ResolveUserFromContext bailout",
			"reason", "no_identity_signal",
			"installation_id", installation.ID,
		)
		return ctx
	}
	log.Debug("ResolveUserFromContext dispatch",
		"installation_id", installation.ID,
		"email_present", id.Email != "",
		"account_present", id.AccountID != "",
	)
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID)
}

// ClaudeCodeMetadata mirrors the JSON Claude Code encodes into
// metadata.user_id. Email is promoted to router.model_router_users.
type ClaudeCodeMetadata struct {
	DeviceID  string `json:"device_id"`
	AccountID string `json:"account_uuid"`
	SessionID string `json:"session_id"`
	Email     string `json:"email"`
}

// ParseClaudeCodeMetadata extracts identity fields from the JSON in
// metadata.user_id. Best-effort: returns zero on parse failure.
func ParseClaudeCodeMetadata(raw string) ClaudeCodeMetadata {
	if raw == "" {
		return ClaudeCodeMetadata{}
	}
	var meta ClaudeCodeMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return ClaudeCodeMetadata{}
	}
	return meta
}

// MaxEmailLen caps email length per RFC 5321 §4.5.3.1.3 (256 bytes).
const MaxEmailLen = 254

// MaxClientIdentifierLen bounds caller-controlled opaque identifiers.
// Claude Code emits ~36-char UUIDs; 128 is overhead with flood-protection.
const MaxClientIdentifierLen = 128

// NormalizeClientIdentifier returns input unchanged when within bounds, else "".
// Rejection (not truncation) keeps shape honest: a truncated identifier looks
// valid but no longer correlates.
func NormalizeClientIdentifier(s string) string {
	if len(s) > MaxClientIdentifierLen {
		return ""
	}
	return s
}

// NormalizeEmail trims, lower-cases, and structurally validates an email
// to match the case-sensitive unique index on (installation_id, email).
// Returns "" when empty, oversized, or malformed. Deliverability is not
// checked: email is an opaque identifier, and the shape check is
// flood-protection.
func NormalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > MaxEmailLen {
		return ""
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return ""
	}
	if strings.IndexByte(s[at+1:], '@') >= 0 {
		return ""
	}
	return s
}
