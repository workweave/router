package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
)

// ClientIdentity holds per-request user identification signals. Email is the
// cross-protocol identity persisted as router.model_router_users.email and
// the only signal reliable enough to link to a Weave account.
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

// ClientIdentityFrom reads the ClientIdentity stashed on ctx, or a zero value when absent.
func ClientIdentityFrom(ctx context.Context) ClientIdentity {
	id, _ := ctx.Value(ClientIdentityContextKey{}).(ClientIdentity)
	return id
}

// ResolveUserFromContext pulls identity signals (email and/or Claude
// account_uuid) from ctx and hands them to auth.Service.ResolveAndStashUser,
// returning the (possibly enriched) ctx. Centralized here so resolution rules
// don't diverge across protocol handlers.
//
// No-op when deps are missing, or when neither email nor account_uuid is set.
// Claude CLI v2.1.x packs only account_uuid (no email), so guarding on email
// alone would defeat the account_uuid attribution path.
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
	log.Info("ResolveUserFromContext dispatch",
		"installation_id", installation.ID,
		"email_present", id.Email != "",
		"account_present", id.AccountID != "",
	)
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID)
}

// ClaudeCodeMetadata mirrors the JSON Claude Code encodes into the Anthropic
// metadata.user_id field. Email is promoted into router.model_router_users;
// the others stay request-scoped.
type ClaudeCodeMetadata struct {
	DeviceID  string `json:"device_id"`
	AccountID string `json:"account_uuid"`
	SessionID string `json:"session_id"`
	Email     string `json:"email"`
}

// ParseClaudeCodeMetadata extracts identity fields from the JSON Claude Code
// packs into metadata.user_id. Best-effort: returns a zero value on parse failure.
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

// MaxEmailLen caps email length per RFC 5321 §4.5.3.1.3 (256 bytes); we use
// 254 to bound row growth from caller-controlled inputs flooding the table.
const MaxEmailLen = 254

// MaxClientIdentifierLen bounds caller-controlled opaque identifiers
// (device_id, session_id). Claude Code emits ~36-char UUIDs; 128 is overhead.
// Flood-protection floor against high-cardinality spam into telemetry storage.
const MaxClientIdentifierLen = 128

// NormalizeClientIdentifier returns the input unchanged when it fits inside
// MaxClientIdentifierLen, else "". Rejection (not truncation) keeps the shape
// honest: a truncated identifier looks valid but no longer correlates.
func NormalizeClientIdentifier(s string) string {
	if len(s) > MaxClientIdentifierLen {
		return ""
	}
	return s
}

// NormalizeEmail trims whitespace, lower-cases, and structurally validates an
// email so it matches the case-sensitive unique index on (installation_id,
// email). Returns "" when empty, oversized, or shaped wrong (missing or
// duplicate '@', empty local-part or domain). Callers treat "" as "no email
// signal" and skip the upsert. Deliverability is not checked: email here is
// an opaque identifier, and the shape check is a flood-protection floor.
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
