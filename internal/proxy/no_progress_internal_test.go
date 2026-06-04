package proxy

import (
	"strconv"
	"testing"
	"time"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sessionKeyFromString(s string) [sessionpin.SessionKeyLen]byte {
	var k [sessionpin.SessionKeyLen]byte
	copy(k[:], []byte(s))
	return k
}

func TestComputeNoProgressFingerprint_StableAcrossCalls(t *testing.T) {
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	a := computeNoProgressFingerprint(d, "explore RSVP files in this repository", 1, "")
	b := computeNoProgressFingerprint(d, "explore RSVP files in this repository", 1, "")
	assert.Equal(t, a, b)
}

func TestComputeNoProgressFingerprint_DistinguishesModel(t *testing.T) {
	d1 := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	d2 := router.Decision{Model: "deepseek/deepseek-v4-flash", Provider: "deepinfra"}
	a := computeNoProgressFingerprint(d1, "explore", 1, "")
	b := computeNoProgressFingerprint(d2, "explore", 1, "")
	assert.NotEqual(t, a, b)
}

func TestComputeNoProgressFingerprint_PromptPrefixOnly(t *testing.T) {
	d := router.Decision{Model: "x", Provider: "y"}
	// Identical 512-byte prefix; suffix differs — must still collide.
	prefix := make([]byte, noProgressPromptPrefix)
	for i := range prefix {
		prefix[i] = 'a'
	}
	a := computeNoProgressFingerprint(d, string(prefix)+"suffix1", 1, "")
	b := computeNoProgressFingerprint(d, string(prefix)+"suffix2", 1, "")
	assert.Equal(t, a, b, "only the first %d bytes of prompt text matter for the fingerprint", noProgressPromptPrefix)
}

func TestComputeNoProgressFingerprint_DistinguishesMessageCount(t *testing.T) {
	d := router.Decision{Model: "gemini-3.1-pro-preview", Provider: "google"}
	// Same model/provider/prompt-prefix/tool-progress, different message count:
	// a tool-free loop grows its transcript each turn, so the fingerprints must
	// diverge even though the user's typed task (the prompt prefix) is constant.
	a := computeNoProgressFingerprint(d, "explore RSVP files", 10, "")
	b := computeNoProgressFingerprint(d, "explore RSVP files", 12, "")
	assert.NotEqual(t, a, b, "a growing message count must change the fingerprint")
}

func TestComputeNoProgressFingerprint_DistinguishesToolProgress(t *testing.T) {
	d := router.Decision{Model: "deepseek/deepseek-v4-flash", Provider: "deepinfra"}
	// Everything constant except the tool-progress marker — the case Claude
	// Code's Explore sub-agent hits, where model/provider/message_count and the
	// user's task prompt are all flat but the tool-call history advances. The
	// fingerprint must diverge so the detector does not mistake progress for a
	// loop.
	a := computeNoProgressFingerprint(d, "investigate the bug", 5, "42\x00Read\x00hash-a")
	b := computeNoProgressFingerprint(d, "investigate the bug", 5, "43\x00Read\x00hash-b")
	assert.NotEqual(t, a, b, "an advancing tool-progress marker must change the fingerprint")
}

func TestNoProgressTracker_DoesNotTripWhenMessageCountGrows(t *testing.T) {
	// Regression: a fast Claude Code tool-call loop fires well over the
	// threshold of dispatches to one model within the window, all sharing the
	// same routed (model, provider) and the same user-task prompt prefix
	// (tool_result turns are stripped from promptText in embed-only mode). It
	// must NOT trip, because the transcript grows by an assistant + tool_result
	// turn each iteration.
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-healthy-loop")
	install := uuid.New()
	d := router.Decision{Model: "gemini-3.1-pro-preview", Provider: "google"}
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold*2; i++ {
		fp := computeNoProgressFingerprint(d, "implement the feature", 4+2*i, "")
		looped, _ := tr.recordAndDetect(key, install, "high", fp, now)
		assert.False(t, looped, "a progressing loop (growing message count) must not trip (call %d)", i)
	}
}

func TestNoProgressTracker_DoesNotTripWhenToolProgressGrows(t *testing.T) {
	// Regression (Claude Code Explore sub-agent): the top-level message count
	// stays flat across turns — tool_use blocks accrete inside a handful of
	// messages — and tool_result turns are stripped from promptText in
	// embed-only mode, so model/provider/message_count/prompt-prefix are all
	// constant. It must NOT trip, because the agent appends a new, distinct tool
	// call each turn, so the tool-progress marker advances.
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-explore")
	install := uuid.New()
	d := router.Decision{Model: "deepseek/deepseek-v4-flash", Provider: "deepinfra"}
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold*2; i++ {
		// Flat message count (5) and constant task prompt, but a growing
		// tool-call history: the count climbs and the last call differs (a Read
		// of a different file) each turn.
		progress := strconv.Itoa(40+i) + "\x00Read\x00hash-" + strconv.Itoa(i)
		fp := computeNoProgressFingerprint(d, "investigate the stuck name", 5, progress)
		looped, _ := tr.recordAndDetect(key, install, "low", fp, now)
		assert.False(t, looped, "a progressing tool-call loop must not trip even with flat message count (call %d)", i)
	}
}

func TestNoProgressTracker_TripsWhenMessageCountAndToolProgressFlat(t *testing.T) {
	// A genuine stuck loop — a sub-agent spawn loop replaying independent
	// envelope-1 requests, or a model re-issuing one identical tool call — never
	// advances: the transcript stays flat and the same single tool call repeats,
	// so both the message count and the tool-progress marker are constant. The
	// detector must still fire.
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-spawn-loop")
	install := uuid.New()
	d := router.Decision{Model: "gemini-3.1-pro-preview", Provider: "google"}
	now := time.Now()
	fp := computeNoProgressFingerprint(d, "spawn sub-agent", 2, "1\x00Agent\x00same-hash")

	var looped bool
	for i := 0; i < noProgressMatchThreshold; i++ {
		looped, _ = tr.recordAndDetect(key, install, "high", fp, now)
	}
	assert.True(t, looped, "a flat-transcript, flat-tool-progress loop must still trip the detector")
}

func TestToolProgressMarker_AdvancesWithToolCalls(t *testing.T) {
	// Two consecutive turns of a healthy Explore-style loop: the second appends
	// a new, distinct tool call (a Read of a different file). The marker must
	// change so the no-progress fingerprint diverges between turns.
	turn1 := []byte(`{"model":"claude-haiku-4-5","max_tokens":256,"messages":[` +
		`{"role":"user","content":"find the bug"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"scim","path":"/a"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"x"}]}` +
		`]}`)
	turn2 := []byte(`{"model":"claude-haiku-4-5","max_tokens":256,"messages":[` +
		`{"role":"user","content":"find the bug"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"scim","path":"/a"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"1","content":"x"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"2","name":"Read","input":{"file_path":"/b/sync_users.go"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"2","content":"y"}]}` +
		`]}`)

	env1, err := translate.ParseAnthropic(turn1)
	require.NoError(t, err)
	env2, err := translate.ParseAnthropic(turn2)
	require.NoError(t, err)

	m1 := toolProgressMarker(env1)
	m2 := toolProgressMarker(env2)
	assert.NotEmpty(t, m1)
	assert.NotEqual(t, m1, m2, "appending a new distinct tool call must advance the progress marker")
}

func TestToolProgressMarker_EmptyWithoutToolCalls(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	assert.Equal(t, "", toolProgressMarker(env), "a tool-free turn has no tool-progress marker")
}

func TestNoProgressTracker_TripsAfterThresholdHits(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-abc")
	install := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")
	now := time.Now()

	for i := 1; i < noProgressMatchThreshold; i++ {
		looped, count := tr.recordAndDetect(key, install, "high", fp, now)
		assert.False(t, looped, "must not trip before threshold (call %d)", i)
		assert.Equal(t, i, count)
	}

	looped, count := tr.recordAndDetect(key, install, "high", fp, now)
	assert.True(t, looped)
	assert.Equal(t, noProgressMatchThreshold, count)
}

func TestNoProgressTracker_DoesNotTripWhenFingerprintsDiffer(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-xyz")
	install := uuid.New()
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold*2; i++ {
		d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
		fp := computeNoProgressFingerprint(d, "prompt-distinct-"+time.Duration(i).String(), 1, "")
		looped, _ := tr.recordAndDetect(key, install, "high", fp, now)
		assert.False(t, looped, "distinct fingerprints must not trip the detector (call %d)", i)
	}
}

func TestNoProgressTracker_AgesOutOldEntries(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-aging")
	install := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")

	old := time.Now().Add(-2 * noProgressTimeWindow)
	for i := 0; i < noProgressMatchThreshold-1; i++ {
		tr.recordAndDetect(key, install, "high", fp, old)
	}

	// One fresh hit: the stale entries should not contribute to the count.
	looped, count := tr.recordAndDetect(key, install, "high", fp, time.Now())
	assert.False(t, looped, "stale entries (outside window) must not push us over the threshold")
	assert.Equal(t, 1, count)
}

func TestNoProgressTracker_ZeroSessionKeyWithInstallationFallsBack(t *testing.T) {
	// Hard-pin paths (Explore SubAgentDispatch when hardPinExplore is on)
	// and routing with pinStore nil leave SessionKey at zero. The detector
	// must still cover them — bucketing by installationID keeps the
	// per-installation isolation while restoring detection coverage.
	tr := newNoProgressTracker()
	var zero [sessionpin.SessionKeyLen]byte
	install := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")
	now := time.Now()

	for i := 1; i < noProgressMatchThreshold; i++ {
		looped, _ := tr.recordAndDetect(zero, install, "high", fp, now)
		assert.False(t, looped, "must not trip before threshold on the installation-fallback path (call %d)", i)
	}
	looped, count := tr.recordAndDetect(zero, install, "high", fp, now)
	assert.True(t, looped, "installation-fallback path must still detect loops")
	assert.Equal(t, noProgressMatchThreshold, count)
}

func TestNoProgressTracker_ZeroSessionKeyAndZeroInstallationIsSkipped(t *testing.T) {
	// Unauthenticated/test paths can have neither anchor — must skip rather
	// than fall back to a global zero-keyed bucket that unrelated traffic
	// would share.
	tr := newNoProgressTracker()
	var zero [sessionpin.SessionKeyLen]byte
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold*2; i++ {
		looped, count := tr.recordAndDetect(zero, uuid.Nil, "high", fp, now)
		assert.False(t, looped, "no anchor available → detector must never trip (call %d)", i)
		assert.Equal(t, 0, count)
	}
}

func TestNoProgressTracker_ZeroSessionDifferentInstallationsAreIsolated(t *testing.T) {
	// Two distinct installations each emit the same fingerprint on the
	// zero-session-key fallback path. Their buckets must not collide.
	tr := newNoProgressTracker()
	var zero [sessionpin.SessionKeyLen]byte
	installA := uuid.New()
	installB := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold-1; i++ {
		tr.recordAndDetect(zero, installA, "high", fp, now)
	}
	// installB shouldn't see installA's history at all.
	looped, count := tr.recordAndDetect(zero, installB, "high", fp, now)
	assert.False(t, looped)
	assert.Equal(t, 1, count)
}

func TestNoProgressTracker_NilReceiverIsNoOp(t *testing.T) {
	var tr *noProgressTracker
	key := sessionKeyFromString("session-nil")
	d := router.Decision{Model: "x", Provider: "y"}
	fp := computeNoProgressFingerprint(d, "p", 1, "")
	looped, count := tr.recordAndDetect(key, uuid.New(), "high", fp, time.Now())
	assert.False(t, looped)
	assert.Equal(t, 0, count)
}

func TestNoProgressTracker_SeparateSessionsDoNotInterfere(t *testing.T) {
	tr := newNoProgressTracker()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt", 1, "")
	now := time.Now()
	install := uuid.New()

	// Two distinct sessions emit the same fingerprint many times; neither
	// should trip because the count is per-session.
	keyA := sessionKeyFromString("session-A")
	keyB := sessionKeyFromString("session-B")

	for i := 0; i < noProgressMatchThreshold-1; i++ {
		tr.recordAndDetect(keyA, install, "high", fp, now)
		tr.recordAndDetect(keyB, install, "high", fp, now)
	}

	// One more hit on each — each has reached the threshold for its own session.
	loopedA, _ := tr.recordAndDetect(keyA, install, "high", fp, now)
	loopedB, _ := tr.recordAndDetect(keyB, install, "high", fp, now)
	require.True(t, loopedA)
	require.True(t, loopedB)
}

func TestNoProgressTracker_DifferentRolesAreSeparateRings(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-roles")
	install := uuid.New()
	d := router.Decision{Model: "x", Provider: "y"}
	fp := computeNoProgressFingerprint(d, "p", 1, "")
	now := time.Now()

	// Same session key, different role → different LRU entry. Filling the
	// "high" ring should not affect the "low" ring.
	for i := 0; i < noProgressMatchThreshold; i++ {
		tr.recordAndDetect(key, install, "high", fp, now)
	}
	looped, count := tr.recordAndDetect(key, install, "low", fp, now)
	assert.False(t, looped)
	assert.Equal(t, 1, count)
}

func TestShortSessionKey_TruncatesAndRedactsZero(t *testing.T) {
	var zero [sessionpin.SessionKeyLen]byte
	assert.Equal(t, "", shortSessionKey(zero), "zero key must produce empty so logs distinguish missing-anchor from real")

	key := sessionKeyFromString("0123456789ABCDEFsomemore")
	got := shortSessionKey(key)
	assert.Len(t, got, 16, "must log only the first 8 bytes (16 hex chars) to limit cross-request correlation")
	assert.NotContains(t, got, "somemore", "tail bytes must not appear in logs")
}

func TestCompactionTracker_DetectsMessageCountDrop(t *testing.T) {
	// Full compaction: messageCount drops sharply, tool-call count also drops.
	ct := newCompactionTracker()
	key := sessionKeyFromString("session-compaction")
	install := uuid.New()

	// First observation: no prior — must return false.
	assert.False(t, ct.checkAndRecord(key, install, "high", 20, 9), "first observation must not report compaction")

	// Both counts grow: progressing session.
	assert.False(t, ct.checkAndRecord(key, install, "high", 22, 10), "growing counts must not report compaction")

	// messageCount drops sharply (full compaction): fire.
	assert.True(t, ct.checkAndRecord(key, install, "high", 3, 0), "sharp messageCount drop must be reported as compaction")

	// Counts stable after compaction.
	assert.False(t, ct.checkAndRecord(key, install, "high", 5, 2), "growing counts after compaction must not report compaction")
}

func TestCompactionTracker_DetectsRollingWindowTrimming(t *testing.T) {
	// Rolling-window trimming (the pattern observed in session 543151ce):
	// messageCount stays flat because old message pairs are swapped for new
	// ones, but the assistant tool-call count shrinks by one per turn as the
	// oldest tool call drops out of the visible window.
	ct := newCompactionTracker()
	key := sessionKeyFromString("session-rolling")
	install := uuid.New()

	// Establish baseline: 10 messages, 9 tool calls.
	ct.checkAndRecord(key, install, "high", 10, 9)

	// messageCount flat (10→10), toolCallCount drops (9→8): fire.
	assert.True(t, ct.checkAndRecord(key, install, "high", 10, 8),
		"flat messageCount + shrinking toolCallCount must be detected as rolling-window trim")
}

func TestCompactionTracker_NoFalsePositiveWhenBothGrow(t *testing.T) {
	ct := newCompactionTracker()
	key := sessionKeyFromString("session-growing")
	install := uuid.New()

	ct.checkAndRecord(key, install, "high", 10, 5)
	assert.False(t, ct.checkAndRecord(key, install, "high", 12, 6), "both counts growing must not fire")
	assert.False(t, ct.checkAndRecord(key, install, "high", 14, 6), "flat toolCallCount (no new tool call) must not fire")
}

func TestCompactionTracker_NilReceiverIsNoOp(t *testing.T) {
	var ct *compactionTracker
	key := sessionKeyFromString("session-nil")
	assert.False(t, ct.checkAndRecord(key, uuid.New(), "high", 5, 3))
}

func TestCompactionTracker_ZeroAnchorsSkipped(t *testing.T) {
	ct := newCompactionTracker()
	var zero [sessionpin.SessionKeyLen]byte
	// No session key, no installation — must skip rather than bucket globally.
	ct.checkAndRecord(zero, uuid.Nil, "high", 10, 5)
	assert.False(t, ct.checkAndRecord(zero, uuid.Nil, "high", 5, 3), "no anchor → compaction detection must be skipped")
}

func TestCompactionTracker_SeparateSessionsAreIsolated(t *testing.T) {
	ct := newCompactionTracker()
	install := uuid.New()
	keyA := sessionKeyFromString("session-A")
	keyB := sessionKeyFromString("session-B")

	ct.checkAndRecord(keyA, install, "high", 20, 9)
	ct.checkAndRecord(keyB, install, "high", 10, 5)

	// Drop on A must not affect B's baseline.
	assert.True(t, ct.checkAndRecord(keyA, install, "high", 5, 3), "drop on A must be detected")
	assert.False(t, ct.checkAndRecord(keyB, install, "high", 12, 6), "B growing from 10→12 must not be compaction")
}
