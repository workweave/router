package proxy

import (
	"crypto/sha256"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// DeriveSessionKey produces the 16-byte digest used to look up a
// session pin (see sessionpin.SessionKeyLen). Three tiers in order:
//
//  1. routerUserID — the resolved router.model_router_users.id. Stable
//     across devices and clients for the same human, so a session that
//     starts on one device and continues on another stays pinned.
//     This is the clean path; clients that send an email get it.
//  2. metadata.user_id — the raw JSON blob Claude Code packs into the
//     Anthropic body. Per-device, but works without an email signal.
//  3. system prompt + first user message. Tracks the same shape
//     Anthropic's prompt cache keys on, so a session that looks
//     identical to the cache also looks identical to the pin store.
//
// apiKeyID is included in all tiers so distinct callers under different
// api keys never collide on a shared pin even if their prompt prefixes
// or user IDs happen to match.
//
// The function is pure (no I/O) — it reads only the arguments. Empty
// apiKeyID and routerUserID are both permitted: anonymous / dev-mode
// requests fall back to the next-most-specific tier.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID, routerUserID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator so dev-mode collisions between caller-controlled
	// strings (apiKeyID, routerUserID, metadata.user_id) cannot produce
	// the same key as a different tier.
	h.Write([]byte{0x00})

	switch {
	case routerUserID != "":
		h.Write([]byte("router_user_id:"))
		h.Write([]byte(routerUserID))
	case env != nil && env.MetadataUserID() != "":
		h.Write([]byte("user_id:"))
		h.Write([]byte(env.MetadataUserID()))
	case env != nil:
		// Stable across turns: system prompt + first user message
		// don't change as the conversation grows. Anthropic's prompt
		// cache keys on the same shape.
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
