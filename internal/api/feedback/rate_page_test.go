package feedback_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	feedbackapi "workweave/router/internal/api/feedback"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWoolyWaveHandler_ServesPNG(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/feedback/assets/wooly-wave.png", feedbackapi.WoolyWaveHandler())

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/assets/wooly-wave.png", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.True(t, len(rec.Body.Bytes()) > 1000, "embedded wooly asset should be non-trivial")
}

func TestWeaveLogoHandler_ServesSVG(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/v1/feedback/assets/weave.svg", feedbackapi.WeaveLogoHandler())

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/feedback/assets/weave.svg", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "<svg")
}
