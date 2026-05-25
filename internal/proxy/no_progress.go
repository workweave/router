package proxy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"sync"
	"time"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

// Cross-envelope tool-call loop detection.
//
// The within-envelope detector in loop_detection.go catches one assistant
// looping inside a single request body. Claude Code's sub-agent failure
// mode is different: the parent agent keeps spawning fresh sub-conversations
// that each make one tool call, get back an identical empty result, and the
// router only sees N independent envelope-1 requests. To the per-envelope
// detector each envelope looks fine in isolation.
//
// noProgressTracker keys an in-process LRU on the inbound session key and
// holds a ring of recent dispatch fingerprints. When the same fingerprint
// appears noProgressMatchThreshold times within noProgressTimeWindow the
// session is declared stuck; the proxy emits a synthetic stop and expires
// the pin so the next user turn re-routes fresh.

const (
	noProgressCacheSize      = 4096
	noProgressCacheTTL       = 5 * time.Minute
	noProgressWindowSize     = 8
	noProgressMatchThreshold = 5
	noProgressTimeWindow     = 90 * time.Second
	noProgressPromptPrefix   = 512
)

// noProgressFingerprint identifies one dispatch attempt by routed model,
// provider, and a stable hash of the prompt prefix.
type noProgressFingerprint [32]byte

func computeNoProgressFingerprint(decision router.Decision, promptText string) noProgressFingerprint {
	p := promptText
	if len(p) > noProgressPromptPrefix {
		p = p[:noProgressPromptPrefix]
	}
	return sha256.Sum256([]byte(decision.Model + "\x00" + decision.Provider + "\x00" + p))
}

type fingerprintEntry struct {
	fp   noProgressFingerprint
	when time.Time
}

type fingerprintRing struct {
	mu      sync.Mutex
	entries []fingerprintEntry
}

func (r *fingerprintRing) recordAndDetect(fp noProgressFingerprint, now time.Time) (looped bool, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, fingerprintEntry{fp: fp, when: now})
	if len(r.entries) > noProgressWindowSize {
		r.entries = r.entries[len(r.entries)-noProgressWindowSize:]
	}
	cutoff := now.Add(-noProgressTimeWindow)
	n := 0
	for _, e := range r.entries {
		if e.fp == fp && !e.when.Before(cutoff) {
			n++
		}
	}
	return n >= noProgressMatchThreshold, n
}

// noProgressTracker is the package-level tracker. Construct via
// newNoProgressTracker; nil receivers are valid (no-op).
type noProgressTracker struct {
	cache *lru.LRU[string, *fingerprintRing]
}

func newNoProgressTracker() *noProgressTracker {
	return &noProgressTracker{
		cache: lru.NewLRU[string, *fingerprintRing](noProgressCacheSize, nil, noProgressCacheTTL),
	}
}

// recordAndDetect records the fingerprint against a bucket keyed by
// sessionKey (preferred) or installationID (fallback) and reports whether
// the burst now exceeds the loop threshold. A nil tracker returns (false, 0)
// so production-style construction can stay optional in tests and
// selfhosted deploys.
//
// Bucket selection:
//   - non-zero sessionKey → per-session bucket (normal path)
//   - zero sessionKey + non-nil installationID → per-installation bucket,
//     used by hard-pin paths (Explore SubAgentDispatch under hardPinExplore)
//     and routing with pinStore nil. Coarser than per-session but still
//     keeps detection coverage; the fingerprint's (model, provider, prompt)
//     tuple distinguishes unrelated work in the same installation from a
//     real loop.
//   - zero sessionKey + nil installationID → no anchor available; skipped
//     to avoid one global zero-key bucket false-positive-tripping across
//     unrelated unauthenticated traffic.
func (t *noProgressTracker) recordAndDetect(sessionKey [sessionpin.SessionKeyLen]byte, installationID uuid.UUID, role string, fp noProgressFingerprint, now time.Time) (looped bool, count int) {
	if t == nil || t.cache == nil {
		return false, 0
	}
	key, ok := noProgressBucketKey(sessionKey, installationID, role)
	if !ok {
		return false, 0
	}
	ring, ringOk := t.cache.Get(key)
	if !ringOk || ring == nil {
		ring = &fingerprintRing{}
		t.cache.Add(key, ring)
	}
	return ring.recordAndDetect(fp, now)
}

// noProgressBucketKey picks the LRU bucket for a dispatch. Returns ok=false
// when neither anchor is available — the caller must skip detection rather
// than falling back to a global zero-keyed bucket.
func noProgressBucketKey(sessionKey [sessionpin.SessionKeyLen]byte, installationID uuid.UUID, role string) (string, bool) {
	if sessionKey != ([sessionpin.SessionKeyLen]byte{}) {
		return "session:" + sessionPinCacheKey(sessionKey, role), true
	}
	if installationID != uuid.Nil {
		return "install:" + installationID.String() + ":" + role, true
	}
	return "", false
}

// shortSessionKey returns the first 16 hex chars (64 bits) of a session key
// for use in Info-level logs. Mirrors the auth.APIKey "KeyPrefix" convention
// (8-char prefix is considered safe) at a wider bit margin so an incident
// triager can still correlate two log lines from the same break event,
// while limiting long-window cross-request correlation across retained logs.
//
// "" for an all-zero key so logs visibly distinguish missing-anchor breaks
// from real-session breaks.
func shortSessionKey(sessionKey [sessionpin.SessionKeyLen]byte) string {
	if sessionKey == ([sessionpin.SessionKeyLen]byte{}) {
		return ""
	}
	return fmt.Sprintf("%x", sessionKey[:8])
}

// handleNoProgressBreak writes a synthetic end_turn response, expires the
// session pin, and returns a non-nil error so caller error-handling
// (telemetry, billing skip) treats it as a failed dispatch.
//
// Mirrors handleToolCallLoopBreak's mechanics — same pin-expiry contract,
// same synthetic-response writer paths — but the message explains the
// cross-envelope nature so a human reading the chat history can tell the
// two break modes apart.
func (s *Service) handleNoProgressBreak(
	ctx context.Context,
	w http.ResponseWriter,
	env *translate.RequestEnvelope,
	count int,
	installationID uuid.UUID,
	sessionKey [sessionpin.SessionKeyLen]byte,
	role string,
	decisionModel string,
	decisionProvider string,
) error {
	log := observability.FromContext(ctx)

	msg := fmt.Sprintf(
		"✦ **Weave Router** → no-progress loop detected: %d consecutive requests under this session routed to `%s` (`%s`) with no observable progress in %s. Stopping this turn and clearing the session pin so the next message re-routes.\n\nIf the task is genuinely incomplete, send a follow-up message describing what's still needed; the router will pick a different model.\n\n",
		count, decisionModel, decisionProvider, noProgressTimeWindow,
	)
	if env.SourceFormat() == translate.FormatOpenAI {
		msg = fmt.Sprintf(
			"Weave Router: no-progress loop detected (%d consecutive routes to %s/%s with no progress in %s). Stopping and clearing the session pin.",
			count, decisionModel, decisionProvider, noProgressTimeWindow,
		)
	}

	log.Info("No-progress loop detected; breaking turn",
		"repeat_count", count,
		"window_size", noProgressWindowSize,
		"time_window", noProgressTimeWindow.String(),
		"decision_model", decisionModel,
		"decision_provider", decisionProvider,
		"session_key_prefix", shortSessionKey(sessionKey),
		"role", role,
	)

	// Skip pin upsert when sessionKey is zero — writing a zero-keyed pin row
	// would create a zombie entry shared by every zero-keyed session in the
	// pin store. Hard-pin paths and selfhosted deploys without a pinStore
	// hit this path and don't need persisted pin expiry anyway.
	if s.pinStore != nil && installationID != uuid.Nil && sessionKey != ([sessionpin.SessionKeyLen]byte{}) {
		expired := sessionpin.Pin{
			SessionKey:     sessionKey,
			Role:           role,
			InstallationID: installationID,
			Provider:       "",
			Model:          "",
			Reason:         "no_progress_loop_break",
			TurnCount:      1,
			PinnedUntil:    time.Now().Add(-time.Second),
		}
		if err := s.pinStore.Upsert(context.Background(), expired); err != nil {
			log.Error("no-progress-break: pin store upsert failed", "err", err)
		}
	}
	if s.pinCache != nil {
		s.pinCache.Remove(sessionPinCacheKey(sessionKey, role))
	}

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg)
	}
}
