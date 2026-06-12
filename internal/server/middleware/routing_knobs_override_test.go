package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runInvalidKnob fires a request at the given path with an out-of-range
// x-weave-routing-alpha header and returns the response status + parsed JSON
// body the middleware aborted with.
func runInvalidKnob(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.WithRoutingKnobsOverride())
	// Catch-all handler — middleware aborts before this runs, but Gin still
	// needs a route registered for the path to be matched.
	engine.Any("/*any", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set(middleware.HeaderAlpha, "1.5")
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body), "response body must be JSON")
	return rr.Code, body
}

// TestRoutingKnobsOverride_EnvelopeOpenAI verifies the OpenAI envelope shape
// is emitted for /v1/chat/completions and /v1/responses, matching the
// hardcoded shape clients expect from OpenAI-compatible endpoints.
func TestRoutingKnobsOverride_EnvelopeOpenAI(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			status, body := runInvalidKnob(t, path)
			assert.Equal(t, http.StatusBadRequest, status)

			errObj, ok := body["error"].(map[string]any)
			require.True(t, ok, "OpenAI envelope must have top-level 'error' object")
			assert.Equal(t, "invalid_request_error", errObj["type"])
			assert.Contains(t, errObj["message"], middleware.HeaderAlpha)
			_, hasParam := errObj["param"]
			_, hasCode := errObj["code"]
			assert.True(t, hasParam, "OpenAI envelope must include 'param' field")
			assert.True(t, hasCode, "OpenAI envelope must include 'code' field")
			_, hasOuterType := body["type"]
			assert.False(t, hasOuterType, "OpenAI envelope must not include outer 'type' (that's the Anthropic shape)")
		})
	}
}

// TestRoutingKnobsOverride_EnvelopeAnthropic verifies the Anthropic envelope
// shape (top-level "type": "error" + nested "error.message") is emitted for
// /v1/messages and /v1/route, matching what the Anthropic handlers return.
func TestRoutingKnobsOverride_EnvelopeAnthropic(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/route"} {
		t.Run(path, func(t *testing.T) {
			status, body := runInvalidKnob(t, path)
			assert.Equal(t, http.StatusBadRequest, status)

			assert.Equal(t, "error", body["type"], "Anthropic envelope must have top-level 'type': 'error'")
			errObj, ok := body["error"].(map[string]any)
			require.True(t, ok, "Anthropic envelope must have nested 'error' object")
			assert.Equal(t, "invalid_request_error", errObj["type"])
			assert.Contains(t, errObj["message"], middleware.HeaderAlpha)
		})
	}
}

// TestRoutingKnobsOverride_EnvelopeGemini verifies the Google-style envelope
// (error.{code,message,status}) is emitted for /v1beta/* routes, matching
// the Gemini handler.
func TestRoutingKnobsOverride_EnvelopeGemini(t *testing.T) {
	status, body := runInvalidKnob(t, "/v1beta/models/gemini-2.5-pro:generateContent")
	assert.Equal(t, http.StatusBadRequest, status)

	errObj, ok := body["error"].(map[string]any)
	require.True(t, ok, "Gemini envelope must have nested 'error' object")
	assert.EqualValues(t, http.StatusBadRequest, errObj["code"], "Gemini envelope echoes HTTP status as numeric 'code'")
	assert.Equal(t, "INVALID_ARGUMENT", errObj["status"], "Gemini envelope must include machine-readable 'status'")
	assert.Contains(t, errObj["message"], middleware.HeaderAlpha)
}
