package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBreakerKey(t *testing.T) {
	assert.NotEqual(t, breakerKey("fireworks", "deepseek-v3"), breakerKey("deepinfra", "deepseek-v3"))
	assert.NotEqual(t, breakerKey("fireworks", "deepseek-v3"), breakerKey("fireworks", "qwen3"))
	assert.Equal(t, breakerKey("fireworks", "deepseek-v3"), breakerKey("fireworks", "deepseek-v3"))
}

func TestBreakerRegistry_NilRegistryAlwaysAllows(t *testing.T) {
	var r *breakerRegistry
	recordOutcome, open := r.allow(breakerKey("fireworks", "deepseek-v3"))
	assert.False(t, open)
	// Must not panic regardless of outcome — a nil registry records nothing.
	recordOutcome(false)
	recordOutcome(true)
}

func TestBreakerRegistry_TripsAfterConsecutiveFailures(t *testing.T) {
	r := newBreakerRegistry()
	key := breakerKey("fireworks", "deepseek-v3")

	for i := 0; i < breakerFailureThreshold; i++ {
		recordOutcome, open := r.allow(key)
		assert.False(t, open, "binding should stay closed before the failure threshold is reached")
		recordOutcome(false)
	}

	_, open := r.allow(key)
	assert.True(t, open, "binding should trip open once consecutive failures reach the threshold")
}

func TestBreakerRegistry_SuccessResetsConsecutiveFailureStreak(t *testing.T) {
	r := newBreakerRegistry()
	key := breakerKey("fireworks", "deepseek-v3")

	for i := 0; i < breakerFailureThreshold-1; i++ {
		recordOutcome, _ := r.allow(key)
		recordOutcome(false)
	}
	recordOutcome, open := r.allow(key)
	assert.False(t, open)
	recordOutcome(true) // resets the consecutive-failure streak

	for i := 0; i < breakerFailureThreshold-1; i++ {
		recordOutcome, open := r.allow(key)
		assert.False(t, open, "one success should have reset the streak, so threshold-1 more failures must not trip it")
		recordOutcome(false)
	}
}

func TestBreakerRegistry_BindingsAreIndependent(t *testing.T) {
	r := newBreakerRegistry()
	failingKey := breakerKey("fireworks", "deepseek-v3")
	otherKey := breakerKey("deepinfra", "deepseek-v3")

	for i := 0; i < breakerFailureThreshold; i++ {
		recordOutcome, _ := r.allow(failingKey)
		recordOutcome(false)
	}

	_, failingOpen := r.allow(failingKey)
	_, otherOpen := r.allow(otherKey)
	assert.True(t, failingOpen)
	assert.False(t, otherOpen, "a different (provider, model) binding must not be affected by another binding's breaker")
}
