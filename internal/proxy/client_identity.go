package proxy

import (
	"context"
	"encoding/json"
	"strings"

	"workweave/router/internal/auth"
)

// ClientIdentity holds per-request user identification signals extracted from
// inbound headers and the Anthropic metadata.user_id body field. Claude Code
// populates DeviceID/AccountID/SessionID; Email is the cross-protocol identity
// the router persists as router.model_router_users.email and the only signal
// reliable enough to link to a Weave account.
type ClientIdentity struct {
	DeviceID  string
	AccountID string
	SessionID string
	Email     string
	UserAgent string
	ClientApp string
}

// ClientIdentityContextKey is the request-context key for client identity.
// The handler layer writes it; the proxy service reads it for OTEL spans
// and the decision sidecar log.
type ClientIdentityContextKey struct{}

// ClientIdentityFrom reads the ClientIdentity stashed on ctx by the handler.
// Returns a zero-value identity when absent.
func ClientIdentityFrom(ctx context.Context) ClientIdentity {
	id, _ := ctx.Value(ClientIdentityContextKey{}).(ClientIdentity)
	return id
}

// ResolveUserFromContext is the glue every inbound handler runs after
// stashing ClientIdentity: pull the email out of ctx, hand it to
// auth.Service.ResolveAndStashUser, return the (possibly enriched) ctx.
//
// Lives here rather than in each handler subpackage so the resolution
// rules (when to skip, what to forward) stay in one place — diverging
// copies between Anthropic / OpenAI / Gemini handlers would silently
// break per-protocol attribution.
//
// No-op when authSvc, installation, or the email on the existing
// ClientIdentity is missing; in those cases ctx is returned unchanged.
func ResolveUserFromContext(ctx context.Context, authSvc *auth.Service, installation *auth.Installation) context.Context {
	if authSvc == nil || installation == nil {
		return ctx
	}
	id := ClientIdentityFrom(ctx)
	if id.Email == "" {
		return ctx
	}
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID)
}

// ClaudeCodeMetadata mirrors the JSON structure Claude Code (and friends) encode
// into the Anthropic metadata.user_id string field. Email is the field we
// promote into router.model_router_users; the others stay request-scoped.
type ClaudeCodeMetadata struct {
	DeviceID  string `json:"device_id"`
	AccountID string `json:"account_uuid"`
	SessionID string `json:"session_id"`
	Email     string `json:"email"`
}

// ParseClaudeCodeMetadata extracts device_id, account_uuid, session_id, and
// email from the JSON string Claude Code packs into metadata.user_id. Returns
// a zero-value struct on any parse failure (best-effort, not request-blocking).
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

// MaxEmailLen is the upper bound on email length we accept into
// router.model_router_users. RFC 5321 §4.5.3.1.3 caps a Mail-From path at
// 256 bytes; we use 254 to be safe and to bound row growth from
// caller-controlled inputs (handlers accept metadata.user_id.email and the
// X-Weave-User-Email header from any authenticated caller, so an unbounded
// shape would let a single API key flood the table with distinct strings).
const MaxEmailLen = 254

// NormalizeEmail trims whitespace, lower-cases, and structurally validates an
// email so it matches the case-sensitive unique index on
// (installation_id, email) without letting a caller-controlled input drive
// unbounded growth in router.model_router_users. Returns "" for any input
// that is empty, longer than MaxEmailLen, missing a single '@', or has an
// empty local-part or domain. Callers treat "" as "no email signal" and skip
// the upsert (see auth.Service.ResolveAndStashUser).
//
// We deliberately do NOT validate deliverability — the email is an opaque
// identifier, not a contact channel. The shape check is a flood-protection
// floor, not RFC 5322 parsing.
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
