package proxy

import (
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	out, overflowed := excludeContextOverflowModels(250_000, 0, 8_000, nil, available)

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

	out, overflowed := excludeContextOverflowModels(10_000, 0, 8_000, nil, available)

	assert.Empty(t, overflowed)
	assert.Nil(t, out, "no additions returns the original (nil) denylist unchanged")
}

// TestExcludeContextOverflowModels_SignatureSavingsOnlyForStrippingTargets is
// the regression for the review finding: base64 thought-signatures are stripped
// before dispatch to a non-Anthropic target but kept for an Anthropic
// passthrough. So the signature savings must be applied only to stripping
// (non-Anthropic-family) models. Here the raw estimate overflows both a 256K
// OSS model and a 200K Anthropic model; the savings pull the OSS model back
// under its window (it never receives the signatures) but must NOT rescue the
// Anthropic model (it does).
func TestExcludeContextOverflowModels_SignatureSavingsOnlyForStrippingTargets(t *testing.T) {
	available := map[string]struct{}{
		"moonshotai/kimi-k2.7": {}, // fireworks → OpenAI-compat, strips signatures, 262144 window
		"claude-haiku-4-5":     {}, // anthropic → keeps signatures, 200K window
	}

	// est+reserve = 268K overflows kimi's 262144 without savings; -20K savings = 248K fits.
	out, overflowed := excludeContextOverflowModels(260_000, 20_000, 8_000, nil, available)

	assert.NotContains(t, overflowed, "moonshotai/kimi-k2.7", "OSS target strips signatures, so the savings keep it under its 256K window")
	assert.Contains(t, overflowed, "claude-haiku-4-5", "Anthropic target keeps signatures, so the savings do not apply and it overflows 200K")
	_, kimiExcluded := out["moonshotai/kimi-k2.7"]
	assert.False(t, kimiExcluded, "stripping target must not be denylisted")
}

// TestSafetyExcludedModels_CatchesPolicyExcludedOverflow is the regression for
// the bypass-safety hole: safetyExcludedModels must denylist a model that
// overflows the context window even when that model is ALSO in the installation
// excluded_models set. The routing-path overflow filter seeds excluded_models as
// its base and skips anything already in it, so the both-excluded model never
// reaches the routing denylist — but the bypass gate still has to block it, or a
// pass-through turn 400s on the subscription. safetyExcludedModels re-runs the
// filter against an empty base to close that gap.
func TestSafetyExcludedModels_CatchesPolicyExcludedOverflow(t *testing.T) {
	// A body large enough that ContextOverflowTokenEstimate (len/6) plus the
	// output reserve exceeds haiku's 200K window. ~1.3MB / 6 ≈ 217K > 200K.
	big := strings.Repeat("x", 1_300_000)
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"` + big + `"}]}`))
	require.NoError(t, err)

	// haiku-4-5 is a 200K-only model (no extended-context beta), so the big body
	// overflows it. It is also the requested model AND policy-excluded here.
	s := &Service{availableModels: map[string]struct{}{"claude-haiku-4-5": {}}}

	// The routing-path filter, seeded with the policy exclusion, skips haiku (it
	// is already excluded) — so the overflow denylist it returns is empty.
	_, routingOverflowed := excludeContextOverflowModels(
		env.ContextOverflowTokenEstimate(), env.SignatureTokenSavings(), 8_000,
		map[string]struct{}{"claude-haiku-4-5": {}}, s.availableModels,
	)
	assert.NotContains(t, routingOverflowed, "claude-haiku-4-5",
		"the routing filter skips a policy-excluded model — this is the gap safetyExcludedModels must close")

	// safetyExcludedModels re-runs against an empty base, so it DOES catch the
	// overflow regardless of policy exclusion.
	safety := s.safetyExcludedModels(env, 8_000)
	_, blocked := safety["claude-haiku-4-5"]
	assert.True(t, blocked, "a policy-excluded model that also overflows must land in the safety set so bypass blocks it")
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
