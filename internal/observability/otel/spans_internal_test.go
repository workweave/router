package otel

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// failingRandRead simulates a crypto/rand.Read failure so tests can exercise
// the math/rand/v2 fallback path in generateTraceID/generateSpanID without
// panicking.
func failingRandRead(_ []byte) (int, error) {
	return 0, errors.New("simulated crypto/rand failure")
}

func TestGenerateTraceID_FallsBackOnCryptoRandFailure(t *testing.T) {
	orig := cryptoRandRead
	cryptoRandRead = failingRandRead
	defer func() { cryptoRandRead = orig }()

	assert.NotPanics(t, func() {
		id := generateTraceID()
		assert.Len(t, id, 16)
	})
}

func TestGenerateSpanID_FallsBackOnCryptoRandFailure(t *testing.T) {
	orig := cryptoRandRead
	cryptoRandRead = failingRandRead
	defer func() { cryptoRandRead = orig }()

	assert.NotPanics(t, func() {
		id := generateSpanID()
		assert.Len(t, id, 8)
	})
}

func TestGenerateTraceID_SucceedsWithRealCryptoRand(t *testing.T) {
	id := generateTraceID()
	assert.Len(t, id, 16)
}

func TestGenerateSpanID_SucceedsWithRealCryptoRand(t *testing.T) {
	id := generateSpanID()
	assert.Len(t, id, 8)
}
