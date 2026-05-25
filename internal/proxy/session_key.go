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

// shortKey returns the first 16 hex chars (64 bits) of a session key for
// log-friendly correlation. Empty for zero keys so log lines on the pre-derive
// path don't show a misleading prefix.
func shortKey(key [sessionpin.SessionKeyLen]byte) string {
	var zero [sessionpin.SessionKeyLen]byte
	if key == zero {
		return ""
	}
	return hex.EncodeToString(key[:8])
}

// bindRequestLogger derives the session key and returns a new context carrying
// a request-scoped logger pre-bound with session_key, request_id, api_key_id,
// and ingress. Downstream code reading via observability.FromContext gets these
// attributes on every line for free, so a single session's path through the
// router can be filtered in Cloud Logging by session_key alone.
//
// Returns the derived session key so callers don't re-derive it for force-model
// or loop-detection paths that already needed it.
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
	return observability.WithLogger(ctx, log), log, key
}

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
