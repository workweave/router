package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBase64SignatureBytes sums the byte length of every base64
// thought-signature payload and ignores everything else.
func TestBase64SignatureBytes(t *testing.T) {
	assert.Equal(t, 0, base64SignatureBytes([]byte(`{"messages":[]}`)), "no signature field")
	assert.Equal(t, 6, base64SignatureBytes([]byte(`{"signature":"ABCabc"}`)), "single payload counts its base64 bytes only")
	assert.Equal(t, 10, base64SignatureBytes([]byte(`{"signature":"AAAA"},{"signature":"BBBBBB"}`)), "two payloads sum (4 + 6)")
	// A marker with no closing quote is not counted (truncated / malformed).
	assert.Equal(t, 0, base64SignatureBytes([]byte(`{"signature":"AAAA`)), "unterminated payload is skipped")
}

// TestContextOverflowTokenEstimate_DenseBody divides content-only bytes by the
// dense-content ratio.
func TestContextOverflowTokenEstimate_DenseBody(t *testing.T) {
	body := []byte(strings.Repeat("x", 400))
	e := &RequestEnvelope{body: body, format: FormatAnthropic}
	assert.Equal(t, 100, e.ContextOverflowTokenEstimate(), "400 dense bytes / 4 = 100 tokens")
}

// TestContextOverflowTokenEstimate_SubtractsSignatures excludes the base64
// signature payload — which is stripped before dispatch to a non-Anthropic
// model — before applying the content ratio.
func TestContextOverflowTokenEstimate_SubtractsSignatures(t *testing.T) {
	sig := strings.Repeat("A", 800)
	body := []byte(`{"content":"` + strings.Repeat("x", 400) + `","signature":"` + sig + `"}`)
	e := &RequestEnvelope{body: body, format: FormatAnthropic}

	withSig := e.ContextOverflowTokenEstimate()
	contentOnly := &RequestEnvelope{body: []byte(`{"content":"` + strings.Repeat("x", 400) + `"}`), format: FormatAnthropic}
	// The 800-byte signature adds 800/4 = 200 tokens if it were counted; it must
	// not, so the two estimates differ only by the non-signature framing bytes.
	assert.Less(t, withSig-contentOnly.ContextOverflowTokenEstimate(), 200, "signature bytes are not counted as content tokens")
}

// TestContextOverflowTokenEstimate_TicketRegression is the regression for the
// 262K-overflow ticket: a signature-light, content-dense ~1.05MB body is a real
// ~263K-token prompt. The old ÷6 estimate (~175K) let it pass the pre-filter
// onto a 256K OSS model, which then hard-400'd on context overflow. The
// strip-aware ÷4 estimate must land above that window so the model is excluded.
func TestContextOverflowTokenEstimate_TicketRegression(t *testing.T) {
	const kimiWindow = 262_143
	body := []byte(strings.Repeat("x", 1_050_000))
	e := &RequestEnvelope{body: body, format: FormatAnthropic}

	assert.Greater(t, e.ContextOverflowTokenEstimate(), kimiWindow, "dense ~263K-token body must estimate above a 256K window")
	assert.Less(t, e.FullTokenEstimate(), kimiWindow, "the old ÷6 estimate undercounted below the window — the bug this fixes")
}
