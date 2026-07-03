package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/api/admin"
	"workweave/router/internal/auth"
	"workweave/router/internal/providers"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAPIKeyRepository is a local in-memory auth.APIKeyRepository (the one in
// package auth_test isn't exported). Only implements methods used by these handlers.
type fakeAPIKeyRepository struct {
	mu     sync.Mutex
	keys   []*auth.APIKey
	nextID int
}

func (f *fakeAPIKeyRepository) Create(_ context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	key := &auth.APIKey{
		ID:             fmt.Sprintf("key-%d", f.nextID),
		InstallationID: params.InstallationID,
		ExternalID:     params.ExternalID,
		Name:           params.Name,
		KeyPrefix:      params.KeyPrefix,
		KeyHash:        params.KeyHash,
		KeySuffix:      params.KeySuffix,
		CreatedBy:      params.CreatedBy,
		CreatedAt:      time.Now(),
	}
	f.keys = append(f.keys, key)
	return key, nil
}

func (f *fakeAPIKeyRepository) GetActiveByHashWithInstallation(context.Context, string) (*auth.APIKey, *auth.Installation, error) {
	return nil, nil, fmt.Errorf("not used by these tests")
}

func (f *fakeAPIKeyRepository) ListForInstallation(_ context.Context, installationID string) ([]*auth.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*auth.APIKey, 0, len(f.keys))
	for _, k := range f.keys {
		if k.InstallationID == installationID && k.DeletedAt == nil {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeAPIKeyRepository) MarkUsed(context.Context, string) error { return nil }

func (f *fakeAPIKeyRepository) SoftDelete(_ context.Context, installationID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k.ID == id && k.InstallationID == installationID && k.DeletedAt == nil {
			now := time.Now()
			k.DeletedAt = &now
			return nil
		}
	}
	return nil
}

func (f *fakeAPIKeyRepository) softDeletedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, k := range f.keys {
		if k.DeletedAt != nil {
			out = append(out, k.ID)
		}
	}
	return out
}

const testInstallationID = "inst-1"

// apiKeysEngine mirrors upsertKeyEngine with the key-lifecycle routes.
func apiKeysEngine(svc *auth.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	inject := func(c *gin.Context) {
		c.Set("router_installation", &auth.Installation{ID: testInstallationID})
	}
	engine.GET("/admin/v1/keys", inject, admin.ListAPIKeysHandler(svc))
	engine.POST("/admin/v1/keys", inject, admin.IssueAPIKeyHandler(svc))
	engine.POST("/admin/v1/keys/:id/rotate", inject, admin.RotateAPIKeyHandler(svc))
	engine.GET("/admin/v1/provider-keys", inject, admin.ListExternalKeysHandler(svc))
	return engine
}

func newAuthServiceForKeyTests(apiKeys auth.APIKeyRepository, externalKeys auth.ExternalAPIKeyRepository) *auth.Service {
	return auth.NewService(nil, apiKeys, externalKeys, nil, auth.NoOpAPIKeyCache{}, nil, func() time.Time { return time.Unix(0, 0) })
}

func TestListAPIKeysHandler_ReturnsKeysForInstallation(t *testing.T) {
	repo := &fakeAPIKeyRepository{}
	svc := newAuthServiceForKeyTests(repo, nil)
	_, _, err := svc.IssueAPIKey(context.Background(), testInstallationID, nil, nil)
	require.NoError(t, err)
	// A key on a different installation must not leak into this installation's list.
	_, _, err = svc.IssueAPIKey(context.Background(), "other-inst", nil, nil)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/keys", nil)
	apiKeysEngine(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Keys []struct {
			ID string `json:"id"`
		} `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Keys, 1, "only the requesting installation's keys must be returned")
}

func TestIssueAPIKeyHandler_ReturnsTokenMatchingFingerprint(t *testing.T) {
	repo := &fakeAPIKeyRepository{}
	svc := newAuthServiceForKeyTests(repo, nil)

	body, _ := json.Marshal(map[string]string{"name": "ci-key"})
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	apiKeysEngine(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp struct {
		Key struct {
			ID        string  `json:"id"`
			Name      *string `json:"name"`
			KeyPrefix string  `json:"key_prefix"`
			KeySuffix string  `json:"key_suffix"`
		} `json:"key"`
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Token)
	require.NotNil(t, resp.Key.Name)
	assert.Equal(t, "ci-key", *resp.Key.Name)

	_, wantPrefix, wantSuffix := auth.APITokenFingerprint(resp.Token)
	assert.Equal(t, wantPrefix, resp.Key.KeyPrefix,
		"the returned key_prefix must match the fingerprint derived from the raw token")
	assert.Equal(t, wantSuffix, resp.Key.KeySuffix,
		"the returned key_suffix must match the fingerprint derived from the raw token")
	assert.True(t, auth.HasAPIKeyPrefix(resp.Token),
		"issued router keys must carry the rk_ prefix")
}

func TestRotateAPIKeyHandler_SoftDeletesOldKeyAndIssuesNew(t *testing.T) {
	repo := &fakeAPIKeyRepository{}
	svc := newAuthServiceForKeyTests(repo, nil)
	oldKey, _, err := svc.IssueAPIKey(context.Background(), testInstallationID, nil, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys/"+oldKey.ID+"/rotate", nil)
	rec := httptest.NewRecorder()
	apiKeysEngine(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp struct {
		Key struct {
			ID string `json:"id"`
		} `json:"key"`
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEqual(t, oldKey.ID, resp.Key.ID, "rotation must issue a new key id")
	assert.NotEmpty(t, resp.Token)
	assert.Contains(t, repo.softDeletedSnapshot(), oldKey.ID,
		"rotation must soft-delete the old key in the repository")
}

func TestRotateAPIKeyHandler_ForeignKeyIDReturnsNotFound(t *testing.T) {
	repo := &fakeAPIKeyRepository{}
	svc := newAuthServiceForKeyTests(repo, nil)
	// Key belongs to a different installation than the one injected by apiKeysEngine.
	foreignKey, _, err := svc.IssueAPIKey(context.Background(), "other-inst", nil, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/keys/"+foreignKey.ID+"/rotate", nil)
	rec := httptest.NewRecorder()
	apiKeysEngine(svc).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code,
		"rotating a key id owned by a foreign installation must map auth.ErrAPIKeyNotFound to 404")
	assert.Empty(t, repo.softDeletedSnapshot(),
		"a rejected rotation must not soft-delete the foreign key")
}

func TestListExternalKeysHandler_ReturnsProviderKeysForInstallation(t *testing.T) {
	externalKey := &auth.ExternalAPIKey{
		ID:             "ext-1",
		InstallationID: testInstallationID,
		Provider:       "anthropic",
		KeyPrefix:      "sk-a",
		KeySuffix:      "test",
	}
	repo := &fakeExternalAPIKeyRepo{keys: []*auth.ExternalAPIKey{externalKey}}
	svc := newAuthServiceForKeyTests(&fakeAPIKeyRepository{}, repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/v1/provider-keys", nil)
	rec := httptest.NewRecorder()
	apiKeysEngine(svc).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var body struct {
		Keys []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
		} `json:"keys"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Keys, 1)
	assert.Equal(t, "ext-1", body.Keys[0].ID)
	assert.Equal(t, "anthropic", body.Keys[0].Provider)
}

// fakeExternalAPIKeyRepo records whether a key was actually persisted, so a test
// can distinguish "guard rejected the write" from "write went through".
type fakeExternalAPIKeyRepo struct {
	created int
	keys    []*auth.ExternalAPIKey
}

func (f *fakeExternalAPIKeyRepo) Create(_ context.Context, params auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	f.created++
	return &auth.ExternalAPIKey{ID: params.ExternalID, Provider: params.Provider}, nil
}
func (f *fakeExternalAPIKeyRepo) GetForInstallation(_ context.Context, installationID string) ([]*auth.ExternalAPIKey, error) {
	out := make([]*auth.ExternalAPIKey, 0, len(f.keys))
	for _, k := range f.keys {
		if k.InstallationID == installationID {
			out = append(out, k)
		}
	}
	return out, nil
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
