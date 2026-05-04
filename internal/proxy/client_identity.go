package proxy

import (
	"context"
	"encoding/json"
)

// ClientIdentity holds per-request user identification signals extracted from
// inbound headers and the Anthropic metadata.user_id body field. Claude Code
// populates all three IDs; other clients may populate a subset.
type ClientIdentity struct {
	DeviceID  string
	AccountID string
	SessionID string
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

// claudeCodeMetadata mirrors the JSON structure Claude Code encodes into the
// Anthropic metadata.user_id string field.
type claudeCodeMetadata struct {
	DeviceID  string `json:"device_id"`
	AccountID string `json:"account_uuid"`
	SessionID string `json:"session_id"`
}

// ParseClaudeCodeMetadata extracts device_id, account_uuid, and session_id
// from the JSON string Claude Code packs into metadata.user_id. Returns
// zero-value fields on any parse failure (best-effort, not request-blocking).
func ParseClaudeCodeMetadata(raw string) (deviceID, accountID, sessionID string) {
	if raw == "" {
		return "", "", ""
	}
	var meta claudeCodeMetadata
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return "", "", ""
	}
	return meta.DeviceID, meta.AccountID, meta.SessionID
}
