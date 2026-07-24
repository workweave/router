package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type modelSelectionTestInstallations struct {
	preferredModels []string
}

func (r *modelSelectionTestInstallations) Create(context.Context, auth.CreateInstallationParams) (*auth.Installation, error) {
	return nil, nil
}

func (r *modelSelectionTestInstallations) Get(context.Context, string, string) (*auth.Installation, error) {
	return nil, nil
}

func (r *modelSelectionTestInstallations) ListForExternalID(context.Context, string) ([]*auth.Installation, error) {
	return nil, nil
}

func (r *modelSelectionTestInstallations) SoftDelete(context.Context, string, string) error {
	return nil
}

func (r *modelSelectionTestInstallations) UpdateExcludedModels(context.Context, string, string, []string) error {
	return nil
}

func (r *modelSelectionTestInstallations) UpdateExcludedProviders(context.Context, string, string, []string) error {
	return nil
}

func (r *modelSelectionTestInstallations) UpdatePreferredModels(_ context.Context, _ string, _ string, models []string) error {
	r.preferredModels = append([]string{}, models...)
	return nil
}

func (r *modelSelectionTestInstallations) UpdateRoutingPreference(context.Context, string, string, *float64) error {
	return nil
}

func (r *modelSelectionTestInstallations) UpdateUsageBypass(context.Context, string, string, bool, *float64) error {
	return nil
}

func (r *modelSelectionTestInstallations) UpdateSubscriptionRoutingDisabled(context.Context, string, string, bool) error {
	return nil
}

type modelSelectionTestAPIKeys struct {
	installation *auth.Installation
}

func (r modelSelectionTestAPIKeys) Create(context.Context, auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return nil, nil
}

func (r modelSelectionTestAPIKeys) GetActiveByHashWithInstallation(context.Context, string) (*auth.APIKey, *auth.Installation, error) {
	return &auth.APIKey{ID: "key-1", InstallationID: r.installation.ID}, r.installation, nil
}

func (r modelSelectionTestAPIKeys) ListForInstallation(context.Context, string) ([]*auth.APIKey, error) {
	return nil, nil
}

func (r modelSelectionTestAPIKeys) MarkUsed(context.Context, string) error {
	return nil
}

func (r modelSelectionTestAPIKeys) SoftDelete(context.Context, string, string) error {
	return nil
}

type modelSelectionTestCatalog []cluster.DeployedEntry

func (c modelSelectionTestCatalog) DefaultDeployedModels() []cluster.DeployedEntry {
	return c
}

func TestModelSelectionHandlers_RKKeyCanUpdatePreferredModels(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installation := &auth.Installation{
		ID:         "installation-1",
		ExternalID: "external-1",
	}
	installations := &modelSelectionTestInstallations{}
	authSvc := auth.NewService(
		installations,
		modelSelectionTestAPIKeys{installation: installation},
		nil,
		nil,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)
	models := modelSelectionTestCatalog{
		{Model: "claude-opus-4-7", Provider: "anthropic"},
		{Model: "gpt-5.5", Provider: "openai"},
	}
	engine := gin.New()
	engine.Use(middleware.WithAdminOrAuth(authSvc, false))
	engine.PUT("/admin/v1/preferred-models", UpdatePreferredModelsHandler(authSvc, models))

	req := httptest.NewRequest(http.MethodPut, "/admin/v1/preferred-models", strings.NewReader(`{"preferred":["gpt-5.5","claude-opus-4-7"]}`))
	req.Header.Set("Authorization", "Bearer rk_test")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	assert.JSONEq(t, `{"preferred":["gpt-5.5","claude-opus-4-7"]}`, recorder.Body.String())
	assert.Equal(t, []string{"gpt-5.5", "claude-opus-4-7"}, installations.preferredModels)
}

func TestGetModelsHandler_ReturnsEnabledState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	installation := &auth.Installation{
		ID:             "installation-1",
		ExternalID:     "external-1",
		ExcludedModels: []string{"gpt-5.5"},
	}
	authSvc := auth.NewService(
		&modelSelectionTestInstallations{},
		modelSelectionTestAPIKeys{installation: installation},
		nil,
		nil,
		auth.NoOpAPIKeyCache{},
		nil,
		time.Now,
	)
	models := modelSelectionTestCatalog{
		{Model: "gpt-5.5", Provider: "openai"},
		{Model: "claude-opus-4-7", Provider: "anthropic"},
	}
	engine := gin.New()
	engine.Use(middleware.WithAdminOrAuth(authSvc, false))
	engine.GET("/admin/v1/models", GetModelsHandler(authSvc, models, nil))

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/models", nil)
	req.Header.Set("Authorization", "Bearer rk_test")
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response []modelStatusDTO
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Equal(t, []modelStatusDTO{
		{Model: "claude-opus-4-7", Provider: "anthropic", Enabled: true},
		{Model: "gpt-5.5", Provider: "openai", Enabled: false},
	}, response)
}
