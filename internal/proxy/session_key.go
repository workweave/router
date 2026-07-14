package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"
)

// shortKey returns the first 16 hex chars of a session key for log
// correlation. Empty for the zero key to avoid a misleading prefix.
func shortKey(key [sessionpin.SessionKeyLen]byte) string {
	var zero [sessionpin.SessionKeyLen]byte
	if key == zero {
		return ""
	}
	return hex.EncodeToString(key[:8])
}

// sessionKeyHex returns the full 32-hex session digest for telemetry joins;
// unlike shortKey, it retains all 16 bytes so parallel threads don't collapse.
func sessionKeyHex(key [sessionpin.SessionKeyLen]byte) string {
	var zero [sessionpin.SessionKeyLen]byte
	if key == zero {
		return ""
	}
	return hex.EncodeToString(key[:])
}

// sessionAffinityHint returns the session key hex for upstream prompt-cache
// stickiness, or "" for the zero key so sessionless requests stay unbucketed.
func sessionAffinityHint(key [sessionpin.SessionKeyLen]byte) string {
	return sessionKeyHex(key)
}

// bindRequestLogger derives the session key and returns a context carrying a
// logger pre-bound with session_key, request_id, api_key_id, and ingress, so
// a session's path can be filtered in Cloud Logging by session_key alone. It
// also returns the key so callers don't have to re-derive it.
func bindRequestLogger(
	ctx context.Context,
	env *translate.RequestEnvelope,
	apiKeyID, requestID, ingress string,
) (context.Context, *slog.Logger, [sessionpin.SessionKeyLen]byte) {
	key := DeriveSessionKey(env, apiKeyID)
	log := observability.FromContext(ctx).With(
		"session_key", shortKey(key),
		"request_id", requestID,
		"api_key_id", apiKeyID,
		"ingress", ingress,
	)
	// The client's own session id (e.g. Claude Code's /status UUID), bound
	// when present so operators can grep by the id the user actually sees.
	if env != nil {
		if cs := env.ClientSessionID(); cs != "" {
			log = log.With("client_session_id", cs)
		}
	}
	return observability.WithLogger(ctx, log), log, key
}

// DeriveSessionKey produces a 16-byte session digest from apiKeyID,
// metadata.user_id (when present), and the first user message.
//
// The first user message is load-bearing: Claude Code's metadata.user_id
// identifies only device+account+session, not the sub-agent, so a main loop
// and all its Task/Explore sub-agents share one user_id. Keying on user_id
// alone collapsed them onto a single pin slot that concurrent threads then
// thrashed (writes/overwrites racing across models). Each thread's first
// user message is stable across turns but distinct per sub-agent, so it
// separates them while keeping each pin (and prompt cache) stable.
//
// System text is excluded on the common Anthropic path because Claude Code
// mutates it every turn, which would re-key (and evict the prompt cache) on
// every request. It's used only as a fallback for OpenAI-format bodies,
// where system lives in messages[] and the first user message is empty.
func DeriveSessionKey(env *translate.RequestEnvelope, apiKeyID string) [sessionpin.SessionKeyLen]byte {
	h := sha256.New()
	h.Write([]byte(apiKeyID))
	// Domain separator prevents cross-tier collisions from caller-controlled strings.
	h.Write([]byte{0x00})

	if env != nil {
		if uid := env.MetadataUserID(); uid != "" {
			h.Write([]byte("user_id:"))
			h.Write([]byte(uid))
			h.Write([]byte{0x00})
		}
		// Fallback for OpenAI-format bodies (see doc comment above): without
		// this, unrelated conversations sharing an API key would collapse.
		disc := env.FirstUserMessageText()
		if disc == "" {
			disc = env.SystemText()
		}
		h.Write([]byte("first_msg:"))
		h.Write([]byte(disc))
	}

	sum := h.Sum(nil)
	var key [sessionpin.SessionKeyLen]byte
	copy(key[:], sum[:sessionpin.SessionKeyLen])
	return key
}
