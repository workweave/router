package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/auth"
	"workweave/router/internal/providers"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExternalAPIKeyRepo records whether a key was actually persisted, so a test
// can distinguish "guard rejected the write" from "write went through".
type fakeExternalAPIKeyRepo struct {
	created int
}

func (f *fakeExternalAPIKeyRepo) Create(_ context.Context, params auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	f.created++
	return &auth.ExternalAPIKey{ID: params.ExternalID, Provider: params.Provider}, nil
}
func (f *fakeExternalAPIKeyRepo) GetForInstallation(context.Context, string) ([]*auth.ExternalAPIKey, error) {
	return nil, nil
}
func (f *fakeExternalAPIKeyRepo) SoftDeleteByProvider(context.Context, string, string) error {
	return nil
}
func (f *fakeExternalAPIKeyRepo) SoftDelete(context.Context, string, string) error { return nil }
func (f *fakeExternalAPIKeyRepo) MarkUsed(context.Context, string) error           { return nil }

// upsertKeyEngine wires UpsertExternalKeyHandler behind a middleware that injects
// an already-authed installation, so the handler reaches the env-shadow guard
// without a real auth flow.
func upsertKeyEngine(svc *auth.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/admin/v1/provider-keys", func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: "inst-1"})
	}, admin.UpsertExternalKeyHandler(svc))
	return engine
}

func postProviderKey(engine *gin.Engine, provider string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"provider": provider, "key": "sk-test-key"})
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/provider-keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestUpsertExternalKeyHandler_RejectsEnvShadowedProvider(t *testing.T) {
	t.Setenv(providers.APIKeyEnvVar(providers.ProviderAnthropic), "sk-ant-deployment-key")

	repo := &fakeExternalAPIKeyRepo{}
	svc := auth.NewService(nil, nil, repo, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Unix(0, 0) })

	rec := postProviderKey(upsertKeyEngine(svc), providers.ProviderAnthropic)

	assert.Equal(t, http.StatusConflict, rec.Code,
		"a provider with a deployment env key must not accept a dashboard BYOK key")
	assert.Equal(t, 0, repo.created,
		"the BYOK key must not be persisted when the env guard fires")
}

func TestUpsertExternalKeyHandler_AllowsProviderWithoutEnvKey(t *testing.T) {
	// No env var set for the provider — the guard must let the write through.
	t.Setenv(providers.APIKeyEnvVar(providers.ProviderAnthropic), "")

	repo := &fakeExternalAPIKeyRepo{}
	svc := auth.NewService(nil, nil, repo, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Unix(0, 0) })

	rec := postProviderKey(upsertKeyEngine(svc), providers.ProviderAnthropic)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, 1, repo.created, "the BYOK key must be persisted when no env key shadows it")
}
