package proxy

import (
	"crypto/sha256"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// DeriveSessionKey produces a 16-byte session digest. Tries metadata.user_id
// (session-distinct from Claude Code's device+account+session bundle), then
// system prompt + first user message (prompt-cache shape fallback). apiKeyID
// is mixed into every tier to prevent cross-key collisions.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator prevents cross-tier collisions from caller-controlled strings.
	h.Write([]byte{0x00})

	switch {
	case env != nil && env.MetadataUserID() != "":
		h.Write([]byte("user_id:"))
		h.Write([]byte(env.MetadataUserID()))
	case env != nil:
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
