package translate

import (
	"encoding/base64"
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

func TestOpenAIReasoningSignatureRoundTrip(t *testing.T) {
	sig := encodeOpenAIReasoningSignature("rs_123", "enc_opaque")
	require.NotEmpty(t, sig)

	id, enc, ok := decodeOpenAIReasoningSignature(sig)
	require.True(t, ok)
	assert.Equal(t, "rs_123", id)
	assert.Equal(t, "enc_opaque", enc)
}

func TestOpenAIReasoningSignatureRejectsUnknownEnvelope(t *testing.T) {
	_, _, ok := decodeOpenAIReasoningSignature("not-base64")
	assert.False(t, ok)

	assert.Empty(t, encodeOpenAIReasoningSignature("", "enc"))
	assert.Empty(t, encodeOpenAIReasoningSignature("rs_123", ""))
}

func TestEmbedOpenAIReasoningSignatureInID_RoundTrip(t *testing.T) {
	// The reasoning envelope rides on the following tool_use id because the
	// Claude Code round-trip drops the thinking block but preserves the id.
	sig := encodeOpenAIReasoningSignature("rs_1", "enc_1")
	id := embedOpenAIReasoningSignatureInID("call_abc", sig)
	require.NotEqual(t, "call_abc", id)
	assert.True(t, strings.HasPrefix(id, "call_abc"))

	clean, got := extractOpenAIReasoningSignatureFromID(id)
	assert.Equal(t, "call_abc", clean, "the upstream call_id must be recovered verbatim")
	assert.Equal(t, sig, got)

	rid, enc, ok := decodeOpenAIReasoningSignature(got)
	require.True(t, ok)
	assert.Equal(t, "rs_1", rid)
	assert.Equal(t, "enc_1", enc)
}

func TestExtractOpenAIReasoningSignatureFromID_NoSuffix(t *testing.T) {
	// An id with no embedded envelope comes back unchanged and signature-less.
	clean, sig := extractOpenAIReasoningSignatureFromID("call_plain")
	assert.Equal(t, "call_plain", clean)
	assert.Empty(t, sig)
}

func TestEmbedOpenAIReasoningSignatureInID_NoOpOnEmpty(t *testing.T) {
	assert.Equal(t, "call_abc", embedOpenAIReasoningSignatureInID("call_abc", ""))
	assert.Empty(t, embedOpenAIReasoningSignatureInID("", "sig"))
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

func TestEncodeSignatureForJSON_PreservesNonASCIISignatureBytes(t *testing.T) {
	// Regression: a real Gemini thoughtSignature is opaque bytes (NOT valid
	// UTF-8). When the value flows through embedSignatureInID → ... →
	// extractSignatureFromID the carrier returns it as a Go string containing
	// raw bytes. If that string is then written to JSON via pw.Str, the SSE
	// JSON writer's `for _, c := range s` loop hits Go's utf8.DecodeRune
	// replacement-char path (every invalid byte sequence becomes U+FFFD); the
	// resulting JSON contains no valid base64, so Google's bytes-typed
	// thought_signature field rejects it at the next turn. Re-encoding
	// non-ASCII signatures through base64 before emit preserves the bytes
	// end-to-end.
	cases := []struct {
		name string
		sig  string
		isAscii bool
	}{
		{"ascii-passthrough", "ANTHROPIC_SIG", true},
		{"base64url-delivered", "QU5USFJPUElDX1NJRw", true},
		{"non-ascii-bytes", "\x12\xf5\x11\x0a\xf2\x11\x01\x11\x4d\x32\x0f\x9e\xb8", false},
		{"mixed", "pre-\x00\xff-post", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeSignatureForJSON(tc.sig)
			if tc.isAscii {
				assert.Equal(t, tc.sig, got, "ascii signatures must pass through unchanged")
			} else {
				_, err := base64.RawURLEncoding.DecodeString(got)
				assert.NoError(t, err, "non-ascii signatures must be re-encoded as valid base64url, got %q", got)
				decoded, _ := base64.RawURLEncoding.DecodeString(got)
				assert.Equal(t, []byte(tc.sig), decoded, "the round-tripped bytes must match the input")
			}
		})
	}
}

// TestPrepareGemini_ThoughtSignatureCarrierSurvivesMultiTurn moved to
// gemini_signature_thought_signature_carrier_external_test.go to satisfy the
// (internal) package boundary: that test calls ParseAnthropic which is the only
// translate-package symbol exposed from outside this file and uses
// mustUnmarshal which lives in gemini_test.go (a translate_test file).
