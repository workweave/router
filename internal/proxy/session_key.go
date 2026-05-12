package proxy

import (
	"crypto/sha256"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// DeriveSessionKey produces the 16-byte digest used to look up a session
// pin. Two tiers tried in order:
//
//  1. metadata.user_id (Claude Code packs device_id + account_uuid +
//     session_id here, so this is session-distinct),
//  2. system prompt + first user message (matches Anthropic prompt-cache
//     shape; session-distinct for clients that don't set metadata.user_id).
//
// apiKeyID is mixed into all tiers so callers under different keys can't
// collide on a shared pin. Pure function; empty apiKeyID just falls back to
// the next tier.
//
// Routing the same human across separate sessions to the same pin would be a
// user pin, not a session pin — so the resolved router user ID is
// deliberately not consulted here.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator: prevents cross-tier collisions from caller-controlled strings.
	h.Write([]byte{0x00})

	switch {
	case env != nil && env.MetadataUserID() != "":
		h.Write([]byte("user_id:"))
		h.Write([]byte(env.MetadataUserID()))
	case env != nil:
		// Stable across turns: matches Anthropic's prompt-cache key shape.
		h.Write([]byte("prompt_prefix:"))
		h.Write([]byte(env.SystemText()))
		h.Write([]byte{0x00})
		h.Write([]byte(env.FirstUserMessageText()))
	}

	sum := h.Sum(nil)
	var key [sessionpin.SessionKeyLen]byte
	copy(key[:], sum[:sessionpin.SessionKeyLen])
	return key
}
