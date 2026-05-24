package proxy

import (
	"testing"
	"time"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"

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
	a := computeNoProgressFingerprint(d, "explore RSVP files in this repository")
	b := computeNoProgressFingerprint(d, "explore RSVP files in this repository")
	assert.Equal(t, a, b)
}

func TestComputeNoProgressFingerprint_DistinguishesModel(t *testing.T) {
	d1 := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	d2 := router.Decision{Model: "deepseek/deepseek-v4-flash", Provider: "deepinfra"}
	a := computeNoProgressFingerprint(d1, "explore")
	b := computeNoProgressFingerprint(d2, "explore")
	assert.NotEqual(t, a, b)
}

func TestComputeNoProgressFingerprint_PromptPrefixOnly(t *testing.T) {
	d := router.Decision{Model: "x", Provider: "y"}
	// Identical 512-byte prefix; suffix differs — must still collide.
	prefix := make([]byte, noProgressPromptPrefix)
	for i := range prefix {
		prefix[i] = 'a'
	}
	a := computeNoProgressFingerprint(d, string(prefix)+"suffix1")
	b := computeNoProgressFingerprint(d, string(prefix)+"suffix2")
	assert.Equal(t, a, b, "only the first %d bytes of prompt text matter for the fingerprint", noProgressPromptPrefix)
}

func TestNoProgressTracker_TripsAfterThresholdHits(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-abc")
	install := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")
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
		fp := computeNoProgressFingerprint(d, "prompt-distinct-"+time.Duration(i).String())
		looped, _ := tr.recordAndDetect(key, install, "high", fp, now)
		assert.False(t, looped, "distinct fingerprints must not trip the detector (call %d)", i)
	}
}

func TestNoProgressTracker_AgesOutOldEntries(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-aging")
	install := uuid.New()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")

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
	fp := computeNoProgressFingerprint(d, "prompt")
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
	fp := computeNoProgressFingerprint(d, "prompt")
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
	fp := computeNoProgressFingerprint(d, "prompt")
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
	fp := computeNoProgressFingerprint(d, "p")
	looped, count := tr.recordAndDetect(key, uuid.New(), "high", fp, time.Now())
	assert.False(t, looped)
	assert.Equal(t, 0, count)
}

func TestNoProgressTracker_SeparateSessionsDoNotInterfere(t *testing.T) {
	tr := newNoProgressTracker()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")
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
	fp := computeNoProgressFingerprint(d, "p")
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
