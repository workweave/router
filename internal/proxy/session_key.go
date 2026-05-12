package proxy

import (
	"crypto/sha256"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// DeriveSessionKey produces the 16-byte digest used to look up a session
// pin. Three tiers tried in order:
//
//  1. routerUserID (stable across devices for the same human),
//  2. metadata.user_id (per-device, no email needed),
//  3. system prompt + first user message (matches Anthropic prompt-cache shape).
//
// apiKeyID is mixed into all tiers so callers under different keys can't
// collide on a shared pin. Pure function; empty apiKeyID/routerUserID just
// fall back to the next tier.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID, routerUserID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator: prevents cross-tier collisions from caller-controlled strings.
	h.Write([]byte{0x00})

	switch {
	case routerUserID != "":
		h.Write([]byte("router_user_id:"))
		h.Write([]byte(routerUserID))
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
