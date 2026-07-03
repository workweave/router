package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
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
// loop_detection.go catches loops within one request body. This catches a
// different failure mode: Claude Code's parent agent spawning fresh
// sub-agents that each make one tool call and get an identical empty
// result — each envelope-1 request looks fine on its own, so the
// per-envelope detector misses it.
//
// noProgressTracker keys an LRU on the session key and rings recent dispatch
// fingerprints; noProgressMatchThreshold repeats within noProgressTimeWindow
// mark the session stuck, so the proxy emits a synthetic stop and expires
// the pin.

const (
	noProgressCacheSize      = 4096
	noProgressCacheTTL       = 5 * time.Minute
	noProgressWindowSize     = 8
	noProgressMatchThreshold = 5
	noProgressTimeWindow     = 90 * time.Second
	noProgressPromptPrefix   = 512
)

// noProgressFingerprint identifies one dispatch by model, provider, message
// count, tool-call progress, and a hash of the prompt prefix.
type noProgressFingerprint [32]byte

// toolProgressMarker summarizes tool-call progress this turn: count of
// assistant tool calls plus the last call's signature (name + arg hash).
// Empty for tool-free turns or Gemini requests (unparsed). Lets the
// no-progress fingerprint tell a progressing loop (marker changes each turn)
// from a stuck one (marker constant), without relying on top-level message
// count, which Claude Code sub-agents hold flat.
func toolProgressMarker(env *translate.RequestEnvelope) string {
	if env == nil {
		return ""
	}
	sigs := env.AssistantToolCallSignatures()
	if len(sigs) == 0 {
		return ""
	}
	last := sigs[len(sigs)-1]
	return strconv.Itoa(len(sigs)) + "\x00" + last.Name + "\x00" + last.InputHash
}

// computeNoProgressFingerprint hashes routed (model, provider), message
// count, a tool-call progress marker, and the prompt prefix.
//
// The prompt prefix alone is constant across a healthy agentic task (it's
// just the user's typed task, tool results stripped), so it can't be the
// sole key — every dispatch would collide. progressMarker is the main
// guard: it advances on real progress and stays constant when stuck, so
// only stuck loops accumulate matching fingerprints. messageCount is a
// secondary signal for tool-free loops, but not sufficient alone: Claude
// Code's Explore sub-agent holds message count flat while genuinely
// progressing.
func computeNoProgressFingerprint(decision router.Decision, promptText string, messageCount int, progressMarker string) noProgressFingerprint {
	p := promptText
	if len(p) > noProgressPromptPrefix {
		p = p[:noProgressPromptPrefix]
	}
	return sha256.Sum256([]byte(decision.Model + "\x00" + decision.Provider + "\x00" + strconv.Itoa(messageCount) + "\x00" + progressMarker + "\x00" + p))
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

// compactionState is per-session state: last-seen message count and
// assistant tool-call count. Either dropping can signal history trimming.
type compactionState struct {
	msgCount      int
	toolCallCount int
}

// compactionMinHistoryMessages is the floor below which a count drop is
// benign, not real trimming: fresh sub-agents open at messageCount=1 (looks
// like a drop vs. a prior sub-agent in the same bucket), and Claude Code's
// Explore sub-agent swings its tool-call count widely on a flat ~3-message
// window. Real compaction/trimming only happens on large conversations,
// well above this floor. Bias toward fewer fires: a missed detection just
// passes the body through unchanged, but a false one corrupts an in-flight
// sub-agent with a lossy summary.
const compactionMinHistoryMessages = 8

// compactionTracker detects Claude Code context-window trimming by comparing
// each turn's message count and tool-call count to the last seen values,
// which can leave a non-Anthropic model unaware of work done in elided turns.
//
// Two independent signals catch two trimming shapes: full compaction (message
// count drops sharply, e.g. 20→3) and rolling-window trimming (message count
// stays flat while tool-call count shrinks by one per turn, e.g. 9→8→7→6→5).
// Checking only message count misses the rolling-window case.
//
// Both signals are gated by compactionMinHistoryMessages (see that constant)
// to avoid false-firing on sub-agent startup and Explore's flat window.
//
// nil receivers are valid (no-op), matching noProgressTracker.
type compactionTracker struct {
	cache *lru.LRU[string, compactionState]
}

func newCompactionTracker() *compactionTracker {
	return &compactionTracker{
		cache: lru.NewLRU[string, compactionState](noProgressCacheSize, nil, noProgressCacheTTL),
	}
}

// checkAndRecord records the session's current counts and reports whether
// either dropped vs. the prior observation. Returns false on first
// observation or when no bucket anchor is available.
func (t *compactionTracker) checkAndRecord(sessionKey [sessionpin.SessionKeyLen]byte, installationID uuid.UUID, role string, messageCount, toolCallCount int) bool {
	if t == nil || t.cache == nil {
		return false
	}
	key, ok := noProgressBucketKey(sessionKey, installationID, role)
	if !ok {
		return false
	}
	last, found := t.cache.Get(key)
	t.cache.Add(key, compactionState{msgCount: messageCount, toolCallCount: toolCallCount})
	if !found {
		return false
	}
	// Full compaction: messageCount dropped from a substantial prior
	// conversation (gated to reject the fresh-sub-agent-at-1 case).
	if messageCount < last.msgCount && last.msgCount >= compactionMinHistoryMessages {
		return true
	}
	// Rolling-window trimming: messageCount flat, tool-call count shrinks
	// (gated to reject Explore's flat ~3-message window).
	if toolCallCount < last.toolCallCount && messageCount >= compactionMinHistoryMessages {
		return true
	}
	return false
}

// recordAndDetect records the fingerprint in a bucket keyed by sessionKey
// (preferred) or installationID (fallback), and reports whether the burst
// exceeds the loop threshold. nil tracker returns (false, 0).
//
// Bucket selection:
//   - sessionKey set → per-session bucket
//   - sessionKey zero, installationID set → per-installation bucket
//     (hard-pin paths, pinStore nil); coarser, but the fingerprint tuple
//     still distinguishes unrelated work
//   - neither set → skipped, to avoid one global bucket false-tripping
//     across unrelated traffic
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

// noProgressBucketKey picks the LRU bucket key. ok=false means no anchor is
// available and the caller must skip detection.
func noProgressBucketKey(sessionKey [sessionpin.SessionKeyLen]byte, installationID uuid.UUID, role string) (string, bool) {
	if sessionKey != ([sessionpin.SessionKeyLen]byte{}) {
		return "session:" + hex.EncodeToString(sessionKey[:]) + ":" + role, true
	}
	if installationID != uuid.Nil {
		return "install:" + installationID.String() + ":" + role, true
	}
	return "", false
}

// shortSessionKey returns the first 8 bytes of a session key hex-encoded, for
// safe use in logs (wider margin than auth.APIKey's 8-char KeyPrefix, still
// short enough to limit cross-request correlation). Empty for an all-zero
// key so logs distinguish missing-anchor breaks from real-session ones.
func shortSessionKey(sessionKey [sessionpin.SessionKeyLen]byte) string {
	if sessionKey == ([sessionpin.SessionKeyLen]byte{}) {
		return ""
	}
	return fmt.Sprintf("%x", sessionKey[:8])
}

// handleNoProgressBreak writes a synthetic end_turn response, expires the
// session pin, and returns a non-nil error so callers treat it as a failed
// dispatch (skips billing/telemetry). Mirrors handleToolCallLoopBreak's
// mechanics but with a message that names the cross-envelope loop mode.
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
	inputTokens int,
) error {
	log := observability.FromContext(ctx)

	msg := fmt.Sprintf(
		"✦ **Weave Router** → No-progress loop detected: %d consecutive requests under this session routed to `%s` (`%s`) with no observable progress in %s. Stopping this turn and clearing the session pin so the next message re-routes.\n\nIf the task is genuinely incomplete, send a follow-up message describing what's still needed; the router will pick a different model.\n\n",
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

	// Skip when sessionKey is zero: a zero-keyed pin row would be a zombie
	// entry shared by every zero-keyed session.
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

	switch env.SourceFormat() {
	case translate.FormatOpenAI:
		return writeSyntheticOpenAIResponse(w, env, msg, inputTokens)
	default:
		return writeSyntheticAnthropicResponse(w, env, msg, inputTokens)
	}
}
