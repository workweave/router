package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestContextWindowForRequest_ExtendedContextModelsReport1M is the premise for
// the overflow filter: a CapExtendedContext model always advertises 1M (the
// proxy injects the context-1m beta when it dispatches), while a 200K-only
// model reports its catalog window.
func TestContextWindowForRequest_ExtendedContextModelsReport1M(t *testing.T) {
	assert.Equal(t, 1_000_000, contextWindowForRequest("claude-opus-4-8"))
	assert.Equal(t, 1_000_000, contextWindowForRequest("claude-sonnet-4-6"))
	assert.Equal(t, 200_000, contextWindowForRequest("claude-haiku-4-5"))
}

// TestExcludeContextOverflowModels_KeepsExtendedContextModel is the regression
// for the debug-session bug: a ~250K-token first request was dispatched to
// Opus at its 200K default and 400'd immediately. Opus must survive the
// pre-filter (it serves at 1M) while a true 200K-only model is excluded.
func TestExcludeContextOverflowModels_KeepsExtendedContextModel(t *testing.T) {
	available := map[string]struct{}{
		"claude-opus-4-8":  {},
		"claude-haiku-4-5": {},
	}

	out, overflowed := excludeContextOverflowModels(250_000, 8_000, nil, available)

	assert.Contains(t, overflowed, "claude-haiku-4-5", "200K-only model overflows a 258K request")
	assert.NotContains(t, overflowed, "claude-opus-4-8", "extended-context model fits at 1M and must stay eligible")
	_, opusExcluded := out["claude-opus-4-8"]
	assert.False(t, opusExcluded, "Opus must not be added to the denylist")
}

// TestExcludeContextOverflowModels_NoOverflowUnderWindow leaves the denylist
// untouched when every model fits.
func TestExcludeContextOverflowModels_NoOverflowUnderWindow(t *testing.T) {
	available := map[string]struct{}{
		"claude-opus-4-8":  {},
		"claude-haiku-4-5": {},
	}

	out, overflowed := excludeContextOverflowModels(10_000, 8_000, nil, available)

	assert.Empty(t, overflowed)
	assert.Nil(t, out, "no additions returns the original (nil) denylist unchanged")
}

// TestShouldEnableExtendedContext gates the 1M-context beta on request size:
// ordinary turns stay on the standard window; a large request trips the beta
// well before the ÷5 estimate's undercount could let it reach the 200K wall.
func TestShouldEnableExtendedContext(t *testing.T) {
	assert.False(t, shouldEnableExtendedContext(20_000, 8_000), "small turn must not opt into the 1M window")
	assert.False(t, shouldEnableExtendedContext(extendedContextTriggerTokens-8_000, 8_000), "exactly at the trigger is not over it")
	assert.True(t, shouldEnableExtendedContext(extendedContextTriggerTokens, 8_000), "estimate above the trigger turns the beta on")
	// A ~250K-real-token request estimates well above the trigger even with the
	// ÷5 undercount, so the beta is enabled before it can 400 on the 200K default.
	assert.True(t, shouldEnableExtendedContext(180_000, 8_000), "near-200K request opts into 1M")
}
