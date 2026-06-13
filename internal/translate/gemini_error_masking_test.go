package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// When a cross-format upstream (notably Gemini native) returns a non-2xx AFTER
// the eager Prelude has already committed message_start, the status can no
// longer change the wire response. Previously the translator parsed the
// upstream error body as SSE (yielding nothing) and finishStream emitted a
// clean message_delta/end_turn — masking the failure as an empty successful
// turn that agent harnesses silently drop. The translator must instead surface
// an Anthropic `error` event so the client sees the failure.
//
// This reproduces the geminiTr -> anthropicTr forwarding on error: geminiTr's
// non-streaming Finalize calls inner.WriteHeader(status) + inner.Write(error
// envelope) after the AnthropicSSETranslator's Prelude already ran.
func driveAnthropicSSEUpstreamError(t *testing.T, status int, errorBody string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	tr := translate.NewAnthropicSSETranslator(rec, "gemini-3.1-pro-preview", nil)
	// Eager Prelude: commits SSE headers + message_start before the upstream
	// produces a byte (the cross-format streaming path).
	require.NoError(t, tr.Prelude(true))
	// Upstream errored: status forwarded, then the (translated) error envelope.
	tr.WriteHeader(status)
	_, err := tr.Write([]byte(errorBody))
	require.NoError(t, err)
	require.NoError(t, tr.Finalize())
	return rec.Body.String()
}

func TestAnthropicSSETranslator_SurfacesUpstreamErrorInsteadOfEndTurn(t *testing.T) {
	// The exact Gemini 400 we observed in prod, after GeminiToOpenAIError
	// reshapes it into an OpenAI-style envelope.
	body := driveAnthropicSSEUpstreamError(t, http.StatusBadRequest,
		`{"error":{"message":"Request contains an invalid argument.","type":"invalid_request_error"}}`)

	assert.Contains(t, body, "event: error", "upstream failure must be surfaced as an error event")
	assert.Contains(t, body, "Request contains an invalid argument.", "upstream message must be relayed")
	assert.Contains(t, body, `"type":"invalid_request_error"`, "400 maps to invalid_request_error")
	// The masking bug: a clean end_turn must NOT be synthesized for a failed turn.
	assert.NotContains(t, body, `"stop_reason":"end_turn"`)
	assert.NotContains(t, body, "event: message_delta")
}

func TestAnthropicSSETranslator_MapsUpstreamStatusToErrorType(t *testing.T) {
	cases := []struct {
		status   int
		body     string
		wantType string
		wantMsg  string
	}{
		{http.StatusServiceUnavailable, `{"error":{"message":"Authentication backend unavailable.","status":"UNAVAILABLE"}}`, "overloaded_error", "Authentication backend unavailable."},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`, "rate_limit_error", "rate limited"},
		{http.StatusInternalServerError, `{}`, "api_error", "upstream provider returned HTTP 500"},
	}
	for _, tc := range cases {
		body := driveAnthropicSSEUpstreamError(t, tc.status, tc.body)
		assert.Contains(t, body, "event: error")
		assert.Contains(t, body, tc.wantType)
		assert.Contains(t, body, tc.wantMsg)
		assert.NotContains(t, body, `"stop_reason":"end_turn"`)
	}
}
