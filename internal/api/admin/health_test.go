package admin_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/api/admin"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type healthCheckerFunc func(context.Context) error

func (f healthCheckerFunc) CheckHealth(ctx context.Context) error {
	return f(ctx)
}

func TestHealthHandlerReportsLiveness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/health", admin.HealthHandler)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/health", nil))

	assert.Equal(t, http.StatusOK, response.Code)
	assert.JSONEq(t, `{"status":"ok"}`, response.Body.String())
}

func TestReadinessHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		checker     admin.HealthChecker
		wantStatus  int
		wantPayload string
	}{
		{
			name:        "no dependency",
			wantStatus:  http.StatusOK,
			wantPayload: `{"status":"ok"}`,
		},
		{
			name: "ready dependency",
			checker: healthCheckerFunc(func(context.Context) error {
				return nil
			}),
			wantStatus:  http.StatusOK,
			wantPayload: `{"status":"ok"}`,
		},
		{
			name: "unready dependency hides internal error",
			checker: healthCheckerFunc(func(context.Context) error {
				return errors.New("Get https://internal-service.run.app/readyz: unavailable")
			}),
			wantStatus:  http.StatusServiceUnavailable,
			wantPayload: `{"status":"unhealthy"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := gin.New()
			engine.GET("/readyz", admin.ReadinessHandler(test.checker))
			response := httptest.NewRecorder()

			engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			assert.Equal(t, test.wantStatus, response.Code)
			assert.JSONEq(t, test.wantPayload, response.Body.String())
			assert.NotContains(t, response.Body.String(), "internal-service")
		})
	}
}
