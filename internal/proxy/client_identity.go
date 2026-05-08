package proxy

import (
	"context"
	"encoding/json"
	"strings"
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

// NormalizeEmail trims whitespace and lower-cases an email so it matches the
// case-sensitive unique index on (installation_id, email). Returns "" when the
// input is empty after trimming.
func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
