package middleware_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeExternalAPIKeyRepository struct {
	byInstallationID map[string][]*auth.ExternalAPIKey
}

func (f *fakeExternalAPIKeyRepository) Create(context.Context, auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	return nil, errors.New("not used")
}

func (f *fakeExternalAPIKeyRepository) GetForInstallation(_ context.Context, installationID string) ([]*auth.ExternalAPIKey, error) {
	return f.byInstallationID[installationID], nil
}

func (f *fakeExternalAPIKeyRepository) SoftDeleteByProvider(context.Context, string, string) error {
	return errors.New("not used")
}

func (f *fakeExternalAPIKeyRepository) SoftDelete(context.Context, string, string) error {
	return errors.New("not used")
}

func (f *fakeExternalAPIKeyRepository) MarkUsed(context.Context, string) error {
	return nil
}

type fakeAPIKeyRepository struct {
	byHash map[string]fakeKeyRow
	mu     sync.Mutex
	used   []string
}

type fakeKeyRow struct {
	apiKey       *auth.APIKey
	installation *auth.Installation
}

func (f *fakeAPIKeyRepository) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return nil, errors.New("not used")
}

func (f *fakeAPIKeyRepository) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	row, ok := f.byHash[keyHash]
	if !ok {
		return nil, nil, sql.ErrNoRows
	}
	return row.apiKey, row.installation, nil
}

func (f *fakeAPIKeyRepository) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	return nil, errors.New("not used")
}

func (f *fakeAPIKeyRepository) MarkUsed(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used = append(f.used, id)
	return nil
}

func (f *fakeAPIKeyRepository) SoftDelete(ctx context.Context, id string) error {
	return errors.New("not used")
}

type fakeInstallationRepository struct{}

func (fakeInstallationRepository) Create(ctx context.Context, params auth.CreateInstallationParams) (*auth.Installation, error) {
	return nil, errors.New("not used")
}

func (fakeInstallationRepository) Get(ctx context.Context, externalID, id string) (*auth.Installation, error) {
	return nil, errors.New("not used")
}

func (fakeInstallationRepository) ListForExternalID(ctx context.Context, externalID string) ([]*auth.Installation, error) {
	return nil, errors.New("not used")
}

func (fakeInstallationRepository) SoftDelete(ctx context.Context, externalID, id string) error {
	return errors.New("not used")
}

func (fakeInstallationRepository) UpdateExcludedModels(ctx context.Context, externalID, id string, models []string) error {
	return errors.New("not used")
}

func (fakeInstallationRepository) UpdateExcludedProviders(ctx context.Context, externalID, id string, providerNames []string) error {
	return errors.New("not used")
}

func (fakeInstallationRepository) UpdateRoutingPreference(ctx context.Context, externalID, id string, qualityWeight *float64) error {
	return errors.New("not used")
}
func (fakeInstallationRepository) UpdateUsageBypass(ctx context.Context, externalID, id string, enabled bool, threshold *float64) error {
	return errors.New("not used")
}

func TestWithAuthPrefersRouterKeyHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_router"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	apiKey := &auth.APIKey{ID: "key-1", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix}
	installation := &auth.Installation{ID: "inst-1", ExternalID: "ext-1"}
	repo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		hash: {apiKey: apiKey, installation: installation},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, repo, nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time {
		return time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	})

	engine := gin.New()
	engine.Use(middleware.WithAuth(svc, false))
	engine.GET("/probe", func(c *gin.Context) {
		assert.Equal(t, installation, middleware.InstallationFrom(c))
		assert.Equal(t, apiKey, middleware.APIKeyFrom(c))
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(middleware.RouterKeyHeader, routerToken)
	req.Header.Set("Authorization", "Bearer anthropic-oauth-token")
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}

func TestWithAuthManagedModeDropsBYOKFromContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_managed"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	apiKey := &auth.APIKey{ID: "key-managed", InstallationID: "inst-managed", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix}
	installation := &auth.Installation{ID: "inst-managed", ExternalID: "ext-managed"}
	repo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		hash: {apiKey: apiKey, installation: installation},
	}}
	// VerifyAPIKey returns this row, so without the managed-mode gate the
	// middleware would stash it on the request context.
	externalRepo := &fakeExternalAPIKeyRepository{byInstallationID: map[string][]*auth.ExternalAPIKey{
		installation.ID: {{ID: "ext-leftover", InstallationID: installation.ID, Provider: "anthropic", Plaintext: []byte("sk-ant-leftover")}},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, repo, externalRepo, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Now() })

	engine := gin.New()
	engine.Use(middleware.WithAuth(svc, true))
	engine.GET("/probe", func(c *gin.Context) {
		assert.Nil(t, c.Request.Context().Value(proxy.ExternalAPIKeysContextKey{}),
			"managed mode must drop BYOK rows at the middleware boundary; a leftover row in the table must not reach the proxy ctx")
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(middleware.RouterKeyHeader, routerToken)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}

func TestWithAuthSelfHostedKeepsBYOKInContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_selfhosted"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	apiKey := &auth.APIKey{ID: "key-self", InstallationID: "inst-self", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix}
	installation := &auth.Installation{ID: "inst-self", ExternalID: "ext-self"}
	repo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		hash: {apiKey: apiKey, installation: installation},
	}}
	externalRepo := &fakeExternalAPIKeyRepository{byInstallationID: map[string][]*auth.ExternalAPIKey{
		installation.ID: {{ID: "ext-byok", InstallationID: installation.ID, Provider: "anthropic", Plaintext: []byte("sk-ant-byok")}},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, repo, externalRepo, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Now() })

	engine := gin.New()
	engine.Use(middleware.WithAuth(svc, false))
	engine.GET("/probe", func(c *gin.Context) {
		v, ok := c.Request.Context().Value(proxy.ExternalAPIKeysContextKey{}).([]*auth.ExternalAPIKey)
		require.True(t, ok, "self-hosted mode must propagate BYOK rows to the proxy ctx")
		require.Len(t, v, 1)
		assert.Equal(t, "anthropic", v[0].Provider)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set(middleware.RouterKeyHeader, routerToken)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}

func TestWithAuthKeepsLegacyBearerFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const routerToken = "rk_router"
	hash, prefix, suffix := auth.APITokenFingerprint(routerToken)
	apiKey := &auth.APIKey{ID: "key-1", KeyHash: hash, KeyPrefix: prefix, KeySuffix: suffix}
	installation := &auth.Installation{ID: "inst-1", ExternalID: "ext-1"}
	repo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		hash: {apiKey: apiKey, installation: installation},
	}}
	svc := auth.NewService(fakeInstallationRepository{}, repo, nil, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Now() })

	engine := gin.New()
	engine.Use(middleware.WithAuth(svc, false))
	engine.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+routerToken)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}
