package translate_test

import (
	"net/http"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const extendedCtxBody = `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`

// TestEnableExtendedContext_InjectsContext1MBeta is the regression for serving a
// CapExtendedContext model at its 200K default and 400ing on a large request:
// the proxy sets EnableExtendedContext, so the emit must add the context-1m
// beta even when the client sent none.
func TestEnableExtendedContext_InjectsContext1MBeta(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(extendedCtxBody))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:           "claude-opus-4-8",
		EnableExtendedContext: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "context-1m-2025-08-07", prep.Headers.Get("anthropic-beta"))
}

// TestEnableExtendedContext_NoOpWithoutCapability guards that the injection is
// gated on the target model: a 200K-only model (Haiku) must never receive the
// beta, since it cannot honor it.
func TestEnableExtendedContext_NoOpWithoutCapability(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel:           "claude-haiku-4-5",
		EnableExtendedContext: true,
	})
	require.NoError(t, err)
	assert.Empty(t, prep.Headers.Get("anthropic-beta"))
}

// TestEnableExtendedContext_DedupesClientBeta verifies the client's own
// context-1m beta is preserved without being duplicated when the proxy also
// enables extended context.
func TestEnableExtendedContext_DedupesClientBeta(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(extendedCtxBody))
	require.NoError(t, err)

	in := http.Header{}
	in.Set("anthropic-beta", "context-1m-2025-08-07")
	prep, err := env.PrepareAnthropic(in, translate.EmitOptions{
		TargetModel:           "claude-opus-4-8",
		EnableExtendedContext: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "context-1m-2025-08-07", prep.Headers.Get("anthropic-beta"))
}

// TestEnableExtendedContext_PreservesOtherBetas appends the context-1m token to
// an existing beta list rather than clobbering it.
func TestEnableExtendedContext_PreservesOtherBetas(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(extendedCtxBody))
	require.NoError(t, err)

	in := http.Header{}
	in.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	prep, err := env.PrepareAnthropic(in, translate.EmitOptions{
		TargetModel:           "claude-opus-4-8",
		EnableExtendedContext: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "interleaved-thinking-2025-05-14,context-1m-2025-08-07", prep.Headers.Get("anthropic-beta"))
}

// TestEnableExtendedContext_OffLeavesHeaderUntouched confirms the flag is the
// switch: with it unset, no beta is synthesized.
func TestEnableExtendedContext_OffLeavesHeaderUntouched(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(extendedCtxBody))
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-opus-4-8"})
	require.NoError(t, err)
	assert.Empty(t, prep.Headers.Get("anthropic-beta"))
}
