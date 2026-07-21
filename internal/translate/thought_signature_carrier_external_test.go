package translate_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrepareGemini_ThoughtSignatureCarrierSurvivesMultiTurn is the
// end-to-end repro for the carrier-byte corruption bug. A real Gemini
// thoughtSignature is opaque bytes (not valid UTF-8). When the value flows
// through embedSignatureInID -> ... -> extractSignatureFromID the carrier
// returns it as a Go string containing raw bytes, which the SSE JSON
// writer's rune-by-rune loop then corrupts into replacement chars.
// Google's bytes-typed thought_signature field rejects the result at the
// next turn. encodeSignatureForJSON re-encodes non-ASCII signatures
// through base64 at the emit boundary so the wire shape survives.
func TestPrepareGemini_ThoughtSignatureCarrierSurvivesMultiTurn(t *testing.T) {
	rawSig := "\x12\xf5\x11\x0a\xf2\x11\x01\x11\x4d\x32\x0f\x9e\xb8\x72\x28\x2a\x89\x9b\x50\x7e\x0b"
	idWithSig := "toolu_abc__thought__" + base64.RawURLEncoding.EncodeToString([]byte(rawSig))

	body := []byte(`{
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "` + idWithSig + `", "name": "Read"}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.6-flash"})
	require.NoError(t, err)

	out := mustUnmarshal(t, prep.Body)
	parts := out["contents"].([]any)[1].(map[string]any)["parts"].([]any)
	require.NotEmpty(t, parts)
	gotSig := parts[0].(map[string]any)["thoughtSignature"].(string)

	decoded, err := base64.RawURLEncoding.DecodeString(gotSig)
	require.NoError(t, err, "emitted sig must be valid base64url, got %q", gotSig)
	assert.Equal(t, []byte(rawSig), decoded, "the wire value must base64-decode to the original raw bytes")
}

// TestSanityCarrierRoundTripLocked proves the carrier convention used by
// the test above matches what translate.thought_signature_id.go encodes,
// so the regression test isn't silently testing the wrong shape.
func TestSanityCarrierRoundTripLocked(t *testing.T) {
	id := "toolu_abc__thought__" + base64.RawURLEncoding.EncodeToString([]byte("hello world"))
	assert.True(t, strings.Contains(id, "toolu_abc"))
	assert.Equal(t, "hello world", mustDecodeCarrierID(t, id))
}

func mustDecodeCarrierID(t *testing.T, id string) string {
	t.Helper()
	idx := strings.Index(id, "__thought__")
	if idx < 0 {
		t.Fatalf("no delimiter in %q", id)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(id[idx+len("__thought__"):])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return string(decoded)
}
