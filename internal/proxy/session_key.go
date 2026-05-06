package proxy

import (
	"crypto/sha256"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// DeriveSessionKey produces the 16-byte digest used to look up a
// session pin (see sessionpin.SessionKeyLen). Two tiers:
//
//  1. If the inbound body carries metadata.user_id (Claude Code packs
//     device/account/session into a single string), key on
//     sha256(api_key_id || metadata.user_id). This is the clean path —
//     clients that opt in get clean per-session pinning.
//  2. Otherwise, key on
//     sha256(api_key_id || system_prompt || first_user_message). This
//     tracks the same shape Anthropic's prompt cache keys on, so a
//     session that looks identical to the cache also looks identical
//     to the pin store.
//
// apiKeyID is included in both tiers so distinct callers under
// different api keys never collide on a shared pin, even if their
// prompt prefixes happen to match.
//
// The function is pure (no I/O) — it reads only from the envelope and
// the apiKeyID argument. Empty apiKeyID is permitted: anonymous /
// dev-mode requests fall into a single shared pin under that envelope's
// prompt prefix, which is the correct degraded behavior.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator so a dev-mode `apiKeyID == metadata.user_id`
	// collision (theoretically possible since both are caller-controlled
	// strings) cannot produce the same key as the prompt-prefix tier.
	h.Write([]byte{0x00})

	if env != nil {
		if userID := env.MetadataUserID(); userID != "" {
			h.Write([]byte("user_id:"))
			h.Write([]byte(userID))
		} else {
			// Stable across turns: system prompt + first user message
			// don't change as the conversation grows. Anthropic's prompt
			// cache keys on the same shape.
			h.Write([]byte("prompt_prefix:"))
			h.Write([]byte(env.SystemText()))
			h.Write([]byte{0x00})
			h.Write([]byte(env.FirstUserMessageText()))
		}
	}

	sum := h.Sum(nil)
	var key [sessionpin.SessionKeyLen]byte
	copy(key[:], sum[:sessionpin.SessionKeyLen])
	return key
}
