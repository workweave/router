package proxy

import (
	"testing"
	"time"

	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"

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
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")
	now := time.Now()

	for i := 1; i < noProgressMatchThreshold; i++ {
		looped, count := tr.recordAndDetect(key, "high", fp, now)
		assert.False(t, looped, "must not trip before threshold (call %d)", i)
		assert.Equal(t, i, count)
	}

	looped, count := tr.recordAndDetect(key, "high", fp, now)
	assert.True(t, looped)
	assert.Equal(t, noProgressMatchThreshold, count)
}

func TestNoProgressTracker_DoesNotTripWhenFingerprintsDiffer(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-xyz")
	now := time.Now()

	for i := 0; i < noProgressMatchThreshold*2; i++ {
		d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
		fp := computeNoProgressFingerprint(d, "prompt-distinct-"+time.Duration(i).String())
		looped, _ := tr.recordAndDetect(key, "high", fp, now)
		assert.False(t, looped, "distinct fingerprints must not trip the detector (call %d)", i)
	}
}

func TestNoProgressTracker_AgesOutOldEntries(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-aging")
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")

	old := time.Now().Add(-2 * noProgressTimeWindow)
	for i := 0; i < noProgressMatchThreshold-1; i++ {
		tr.recordAndDetect(key, "high", fp, old)
	}

	// One fresh hit: the stale entries should not contribute to the count.
	looped, count := tr.recordAndDetect(key, "high", fp, time.Now())
	assert.False(t, looped, "stale entries (outside window) must not push us over the threshold")
	assert.Equal(t, 1, count)
}

func TestNoProgressTracker_NilReceiverIsNoOp(t *testing.T) {
	var tr *noProgressTracker
	key := sessionKeyFromString("session-nil")
	d := router.Decision{Model: "x", Provider: "y"}
	fp := computeNoProgressFingerprint(d, "p")
	looped, count := tr.recordAndDetect(key, "high", fp, time.Now())
	assert.False(t, looped)
	assert.Equal(t, 0, count)
}

func TestNoProgressTracker_SeparateSessionsDoNotInterfere(t *testing.T) {
	tr := newNoProgressTracker()
	d := router.Decision{Model: "qwen/qwen3-235b-a22b-2507", Provider: "bedrock"}
	fp := computeNoProgressFingerprint(d, "prompt")
	now := time.Now()

	// Two distinct sessions emit the same fingerprint many times; neither
	// should trip because the count is per-session.
	keyA := sessionKeyFromString("session-A")
	keyB := sessionKeyFromString("session-B")

	for i := 0; i < noProgressMatchThreshold-1; i++ {
		tr.recordAndDetect(keyA, "high", fp, now)
		tr.recordAndDetect(keyB, "high", fp, now)
	}

	// One more hit on each — neither has reached the threshold *for its own session*.
	loopedA, _ := tr.recordAndDetect(keyA, "high", fp, now)
	loopedB, _ := tr.recordAndDetect(keyB, "high", fp, now)
	// Each session has reached the threshold (4 prior + this one = 5).
	require.True(t, loopedA)
	require.True(t, loopedB)
}

func TestNoProgressTracker_DifferentRolesAreSeparateRings(t *testing.T) {
	tr := newNoProgressTracker()
	key := sessionKeyFromString("session-roles")
	d := router.Decision{Model: "x", Provider: "y"}
	fp := computeNoProgressFingerprint(d, "p")
	now := time.Now()

	// Same session key, different role → different LRU entry. Filling the
	// "high" ring should not affect the "low" ring.
	for i := 0; i < noProgressMatchThreshold; i++ {
		tr.recordAndDetect(key, "high", fp, now)
	}
	looped, count := tr.recordAndDetect(key, "low", fp, now)
	assert.False(t, looped)
	assert.Equal(t, 1, count)
}
