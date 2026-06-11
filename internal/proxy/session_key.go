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

// sessionAffinityHint returns the full hex session key for use as an upstream
// prompt-cache stickiness hint (translate.EmitOptions.SessionAffinity), or ""
// for the zero key so requests with no derivable session don't all collapse
// onto one synthetic affinity bucket.
func sessionAffinityHint(key [sessionpin.SessionKeyLen]byte) string {
	var zero [sessionpin.SessionKeyLen]byte
	if key == zero {
		return ""
	}
	return hex.EncodeToString(key[:])
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
	// client_session_id is the calling client's own session id (e.g. Claude
	// Code's /status UUID). Bound when the client surfaces one so operators
	// can grep logs by the id the user actually sees.
	if env != nil {
		if cs := env.ClientSessionID(); cs != "" {
			log = log.With("client_session_id", cs)
		}
	}
	return observability.WithLogger(ctx, log), log, key
}

// DeriveSessionKey produces a 16-byte session digest from metadata.user_id
// (when present) AND the first user message, plus apiKeyID to prevent
// cross-key collisions.
//
// The first user message is load-bearing, not decoration: Claude Code packs
// only its device+account+session bundle into metadata.user_id — NOT sub-agent
// identity — so the main loop and every Task/Explore sub-agent of one session
// share an identical user_id. Keying on user_id alone collapsed them onto a
// single pin slot that concurrent threads then thrashed (one writes haiku, a
// sibling reuses it, a third overwrites qwen…), producing the cross-model
// bouncing operators saw. A thread's first user message is stable across its
// turns and distinct per sub-agent (each carries its own dispatch prompt), so
// it separates the threads while keeping each one's pin (and prompt cache)
// stable.
//
// System text is deliberately excluded for the common (Anthropic) path: Claude
// Code mutates the system prompt every turn (verified in repro — a fresh hash
// each request), so folding it in would re-key every turn and evict the prompt
// cache it exists to preserve. It is used only as a fallback for OpenAI-format
// bodies, whose system lives inside messages[] and leaves the first user
// message empty — without the fallback such conversations would collapse onto
// one pin.
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
		// OpenAI-format bodies carry `system` inside messages[], so their first
		// message is often a system role and FirstUserMessageText is empty.
		// Fall back to system text there so unrelated conversations that share
		// an API key (and lack metadata.user_id) don't collapse onto one pin.
		// Anthropic (Claude Code) keeps system out of messages[], so the first
		// user message is populated and the volatile system prompt stays out.
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
