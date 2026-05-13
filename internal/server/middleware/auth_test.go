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
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func (fakeInstallationRepository) UpdateExcludedModels(ctx context.Context, id string, models []string) error {
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
	engine.Use(middleware.WithAuth(svc))
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
	engine.Use(middleware.WithAuth(svc))
	engine.GET("/probe", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+routerToken)
	rr := httptest.NewRecorder()
	engine.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
}
