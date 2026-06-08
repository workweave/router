package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeToolUseID_PreservesLongThoughtSignatureID(t *testing.T) {
	// Regression: sanitizeToolUseID is shared by the Anthropic and Gemini emit
	// paths. A Gemini thoughtSignature smuggled into the id makes it >64 bytes;
	// it must survive verbatim so extractSignatureFromID can recover it on the
	// next Gemini turn. Length must NOT be clamped here.
	id := embedSignatureInID("toolu_abc", strings.Repeat("sig", 100))
	require.Greater(t, len(id), maxToolCallIDLen)
	assert.Equal(t, id, sanitizeToolUseID(id), "thought-signature id preserved on the shared path")

	_, sig := extractSignatureFromID(sanitizeToolUseID(id))
	assert.NotEmpty(t, sig, "thoughtSignature still recoverable after sanitize")
}

func TestClampOpenAIToolCallID(t *testing.T) {
	// Short id: unchanged.
	assert.Equal(t, "toolu_abc", clampOpenAIToolCallID("toolu_abc"))

	// Thought-signature id with a short base: signature dropped (OpenAI can't
	// use it), bare id kept and within the limit.
	withSig := embedSignatureInID("toolu_abc", strings.Repeat("sig", 100))
	assert.Equal(t, "toolu_abc", clampOpenAIToolCallID(withSig))

	// Degenerate over-length id (the 1411-char failure): hashed to <=64.
	long := "toolu_" + strings.Repeat("a", 1411)
	got := clampOpenAIToolCallID(long)
	assert.LessOrEqual(t, len(got), maxToolCallIDLen)
	assert.NotEqual(t, long, got)
	assert.True(t, strings.HasPrefix(got, "tc_"))

	// Pairing: the tool_use id and its tool_result id carry the same original
	// string, so they must clamp to the same value.
	assert.Equal(t, got, clampOpenAIToolCallID(long))
}
