package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/router"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runForceEffort fires a request at the given path with the supplied
// x-weave-effort header. Returns the response status + canonical level
// captured off the request context (when the middleware lets the request
// through) or the abort envelope when it 400s.
func runForceEffort(t *testing.T, path, header string) (status int, body any, captured *router.Overrides) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.WithForceEffortOverride())
	engine.Any("/*any", func(c *gin.Context) {
		// Capture the override the middleware stashed on ctx so the test
		// can assert the value flows through unchanged.
		if k := router.RoutingKnobsFromContext(c.Request.Context()); k != nil {
			captured = k
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, path, nil)
	if header != "" {
		req.Header.Set(middleware.ForceEffortOverrideHeader, header)
	}
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	var parsed any
	if rr.Body.Len() > 0 {
		_ = json.Unmarshal(rr.Body.Bytes(), &parsed)
	}
	return rr.Code, parsed, captured
}

// TestForceEffortOverride_BareValue verifies x-weave-effort low passes
// through to Overrides.ForceEffort in canonical (low) form.
func TestForceEffortOverride_BareValue(t *testing.T) {
	status, _, captured := runForceEffort(t, "/v1/messages", "low")
	assert.Equal(t, http.StatusOK, status)
	require.NotNil(t, captured)
	assert.Equal(t, "low", captured.ForceEffort)
}

// TestForceEffortOverride_AliasCanonicalizes confirms alias forms
// (`fast`, `minimal`, `ultra`) map to canonical wire strings before stash.
func TestForceEffortOverride_AliasCanonicalizes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"fast", "low"},
		{"minimal", "low"},
		{"ultra", "xhigh"},
		{"ULTRA", "xhigh"}, // case-insensitive
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			status, _, captured := runForceEffort(t, "/v1/messages", tc.in)
			assert.Equal(t, http.StatusOK, status)
			require.NotNil(t, captured, "middleware must stash parsed override")
			assert.Equal(t, tc.want, captured.ForceEffort)
		})
	}
}

// TestForceEffortOverride_InvalidValue verifies the middleware 400s
// on an unknown level instead of letting it through to a provider that
// would 400 with the wrong-shape envelope (Anthropic / OpenAI / Gemini
// all parse their own error shapes — easier to fail at the boundary).
func TestForceEffortOverride_InvalidValue(t *testing.T) {
	status, _, captured := runForceEffort(t, "/v1/messages", "garbage")
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Nil(t, captured, "invalid value must not stash an override")
}

// TestForceEffortOverride_Absent confirms an empty header is a no-op
// (the middleware lets the request through without stashing).
func TestForceEffortOverride_Absent(t *testing.T) {
	status, _, captured := runForceEffort(t, "/v1/messages", "")
	assert.Equal(t, http.StatusOK, status)
	assert.Nil(t, captured)
}
