package auth_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeAPIKeyRepository struct {
	byHash   map[string]fakeKeyRow
	override error

	mu       sync.Mutex
	markUsed []string
}

func (f *fakeAPIKeyRepository) markUsedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.markUsed))
	copy(out, f.markUsed)
	return out
}

type fakeKeyRow struct {
	apiKey       *auth.APIKey
	installation *auth.Installation
}

func (f *fakeAPIKeyRepository) Create(ctx context.Context, params auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeAPIKeyRepository) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	if f.override != nil {
		return nil, nil, f.override
	}
	row, ok := f.byHash[keyHash]
	if !ok {
		return nil, nil, sql.ErrNoRows
	}
	return row.apiKey, row.installation, nil
}

func (f *fakeAPIKeyRepository) ListForInstallation(ctx context.Context, installationID string) ([]*auth.APIKey, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeAPIKeyRepository) MarkUsed(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markUsed = append(f.markUsed, id)
	return nil
}

func (f *fakeAPIKeyRepository) SoftDelete(ctx context.Context, id string) error {
	return errors.New("not used by these tests")
}

type fakeExternalAPIKeyRepo struct {
	keys []*auth.ExternalAPIKey
	err  error
}

func (f *fakeExternalAPIKeyRepo) Create(ctx context.Context, params auth.CreateExternalAPIKeyParams) (*auth.ExternalAPIKey, error) {
	return nil, nil
}

func (f *fakeExternalAPIKeyRepo) GetForInstallation(ctx context.Context, installationID string) ([]*auth.ExternalAPIKey, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.keys, nil
}

func (f *fakeExternalAPIKeyRepo) SoftDeleteByProvider(ctx context.Context, installationID, provider string) error {
	return nil
}

func (f *fakeExternalAPIKeyRepo) SoftDelete(ctx context.Context, installationID, id string) error {
	return nil
}

func (f *fakeExternalAPIKeyRepo) MarkUsed(ctx context.Context, id string) error {
	return nil
}

type fakeInstallationRepository struct {
	excludedModelsByID         map[string][]string
	excludedModelsExternalByID map[string]string
}

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
func (f *fakeInstallationRepository) UpdateExcludedModels(ctx context.Context, externalID, id string, models []string) error {
	if f.excludedModelsByID == nil {
		f.excludedModelsByID = map[string][]string{}
	}
	if f.excludedModelsExternalByID == nil {
		f.excludedModelsExternalByID = map[string]string{}
	}
	f.excludedModelsByID[id] = append([]string{}, models...)
	f.excludedModelsExternalByID[id] = externalID
	return nil
}

func frozenClock() auth.Clock {
	t := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func makeService(t *testing.T, rows ...fakeKeyRow) (*auth.Service, *fakeAPIKeyRepository) {
	t.Helper()
	apiKeys := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}}
	for _, row := range rows {
		apiKeys.byHash[row.apiKey.KeyHash] = row
	}
	svc := auth.NewService(
		&fakeInstallationRepository{},
		apiKeys,
		nil,
		nil,
		auth.NoOpAPIKeyCache{},
		nil,
		frozenClock(),
	)
	return svc, apiKeys
}

type recordingAPIKeyCache struct {
	mu            sync.Mutex
	store         map[string]auth.CachedKey
	byInst        map[string]map[string]struct{}
	hits          int
	sets          []recordedSet
	invalidations []string
}

type recordedSet struct {
	keyHash string
	entry   auth.CachedKey
}

func newRecordingAPIKeyCache() *recordingAPIKeyCache {
	return &recordingAPIKeyCache{
		store:  map[string]auth.CachedKey{},
		byInst: map[string]map[string]struct{}{},
	}
}

func (c *recordingAPIKeyCache) Get(keyHash string) (auth.CachedKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.store[keyHash]
	if ok {
		c.hits++
	}
	return v, ok
}

func (c *recordingAPIKeyCache) Set(keyHash string, entry auth.CachedKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[keyHash] = entry
	c.sets = append(c.sets, recordedSet{keyHash: keyHash, entry: entry})
	if !entry.Negative && entry.Installation != nil && entry.Installation.ID != "" {
		hashes, ok := c.byInst[entry.Installation.ID]
		if !ok {
			hashes = map[string]struct{}{}
			c.byInst[entry.Installation.ID] = hashes
		}
		hashes[keyHash] = struct{}{}
	}
}

func (c *recordingAPIKeyCache) InvalidateInstallation(installationID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.invalidations = append(c.invalidations, installationID)
	hashes := c.byInst[installationID]
	delete(c.byInst, installationID)
	for hash := range hashes {
		entry, ok := c.store[hash]
		if !ok || entry.Negative {
			continue
		}
		delete(c.store, hash)
	}
}

func (c *recordingAPIKeyCache) invalidationSnapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.invalidations))
	copy(out, c.invalidations)
	return out
}

func (c *recordingAPIKeyCache) hitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func (c *recordingAPIKeyCache) setSnapshot() []recordedSet {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]recordedSet, len(c.sets))
	copy(out, c.sets)
	return out
}

type repoCallCounter struct {
	*fakeAPIKeyRepository
	mu       sync.Mutex
	getCalls int
}

func (r *repoCallCounter) GetActiveByHashWithInstallation(ctx context.Context, keyHash string) (*auth.APIKey, *auth.Installation, error) {
	r.mu.Lock()
	r.getCalls++
	r.mu.Unlock()
	return r.fakeAPIKeyRepository.GetActiveByHashWithInstallation(ctx, keyHash)
}

func (r *repoCallCounter) getCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getCalls
}

func TestService_VerifyAPIKey_RejectsTokenWithWrongPrefix(t *testing.T) {
	svc, apiKeys := makeService(t)

	_, _, _, err := svc.VerifyAPIKey(context.Background(), "wcckey_irrelevant")

	require.ErrorIs(t, err, auth.ErrInvalidPrefix,
		"non-rk_ tokens must be rejected with ErrInvalidPrefix")
	assert.Empty(t, apiKeys.markUsedSnapshot(),
		"VerifyAPIKey must not touch the api_keys repo when the prefix is wrong")
}

func TestService_VerifyAPIKey_RejectsEmptyToken(t *testing.T) {
	svc, _ := makeService(t)

	_, _, _, err := svc.VerifyAPIKey(context.Background(), "")

	require.ErrorIs(t, err, auth.ErrInvalidPrefix,
		"empty tokens must be rejected with ErrInvalidPrefix (no prefix)")
}

func TestService_VerifyAPIKey_RejectsUnknownToken(t *testing.T) {
	svc, _ := makeService(t)

	_, _, _, err := svc.VerifyAPIKey(context.Background(), "rk_unknown")

	require.ErrorIs(t, err, auth.ErrInvalidToken,
		"a rk_ token whose hash isn't in the repo must return ErrInvalidToken")
}

func TestService_VerifyAPIKey_PropagatesNonNotFoundRepoError(t *testing.T) {
	svc, apiKeys := makeService(t)
	repoErr := errors.New("postgres connection refused")
	apiKeys.override = repoErr

	_, _, _, err := svc.VerifyAPIKey(context.Background(), "rk_anything")

	require.Error(t, err)
	assert.NotErrorIs(t, err, auth.ErrInvalidToken,
		"infrastructure failures must surface as their own errors, not as ErrInvalidToken")
	assert.NotErrorIs(t, err, auth.ErrInvalidPrefix)
	assert.Contains(t, err.Error(), "postgres connection refused",
		"original repo error must propagate so on-call can identify infrastructure failures")
}

func TestService_VerifyAPIKey_HappyPathReturnsInstallationAndKey(t *testing.T) {
	rawToken := "rk_demo_token_for_test_only"
	keyHash := auth.HashAPIKeySHA256(rawToken)

	wantInstall := &auth.Installation{
		ID:         "install_abc",
		ExternalID: "org_acme",
		Name:       "production-api",
		CreatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	wantKey := &auth.APIKey{
		ID:             "key_xyz",
		InstallationID: wantInstall.ID,
		ExternalID:     wantInstall.ExternalID,
		KeyPrefix:      "rk_",
		KeyHash:        keyHash,
		KeySuffix:      "only",
		CreatedAt:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	svc, apiKeys := makeService(t, fakeKeyRow{apiKey: wantKey, installation: wantInstall})

	gotInstall, gotKey, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.NoError(t, err)
	require.NotNil(t, gotInstall)
	require.NotNil(t, gotKey)

	assert.Equal(t, "production-api", gotInstall.Name)
	assert.Equal(t, "org_acme", gotInstall.ExternalID)
	assert.Equal(t, "key_xyz", gotKey.ID)
	assert.Equal(t, keyHash, gotKey.KeyHash)

	require.Eventually(t, func() bool {
		snap := apiKeys.markUsedSnapshot()
		return len(snap) == 1 && snap[0] == "key_xyz"
	}, 500*time.Millisecond, 10*time.Millisecond,
		"VerifyAPIKey must asynchronously call MarkUsed with the matched key id")
}

func makeServiceWithCacheAndCounter(t *testing.T, cache auth.APIKeyCache, rows ...fakeKeyRow) (*auth.Service, *repoCallCounter) {
	t.Helper()
	bare := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}}
	for _, row := range rows {
		bare.byHash[row.apiKey.KeyHash] = row
	}
	counter := &repoCallCounter{fakeAPIKeyRepository: bare}
	svc := auth.NewService(
		&fakeInstallationRepository{},
		counter,
		nil,
		nil,
		cache,
		nil,
		frozenClock(),
	)
	return svc, counter
}

func TestService_VerifyAPIKey_CacheHitSkipsRepo(t *testing.T) {
	rawToken := "rk_cache_hit_test"
	keyHash := auth.HashAPIKeySHA256(rawToken)
	wantInstall := &auth.Installation{
		ID:         "install_cached",
		ExternalID: "org_cached",
		Name:       "cached-tenant",
	}
	wantKey := &auth.APIKey{
		ID:             "key_cached",
		InstallationID: wantInstall.ID,
		ExternalID:     wantInstall.ExternalID,
		KeyHash:        keyHash,
	}

	cache := newRecordingAPIKeyCache()
	cache.Set(keyHash, auth.CachedKey{APIKey: wantKey, Installation: wantInstall})

	svc, repo := makeServiceWithCacheAndCounter(t, cache, fakeKeyRow{apiKey: wantKey, installation: wantInstall})

	gotInstall, gotKey, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.NoError(t, err)
	assert.Equal(t, "install_cached", gotInstall.ID,
		"a cache hit must return the cached installation, not a freshly-fetched one")
	assert.Equal(t, "key_cached", gotKey.ID,
		"a cache hit must return the cached api key")
	assert.Equal(t, 0, repo.getCallCount(),
		"a cache hit must short-circuit the DB lookup; GetActiveByHashWithInstallation must NOT be called")
	assert.Equal(t, 1, cache.hitCount(),
		"VerifyAPIKey must consult the cache exactly once per call")
}

func TestService_VerifyAPIKey_NegativeCacheHitSkipsRepo(t *testing.T) {
	rawToken := "rk_known_bad_token"
	keyHash := auth.HashAPIKeySHA256(rawToken)

	cache := newRecordingAPIKeyCache()
	cache.Set(keyHash, auth.CachedKey{Negative: true})

	svc, repo := makeServiceWithCacheAndCounter(t, cache)

	_, _, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.ErrorIs(t, err, auth.ErrInvalidToken,
		"a negative cache hit must return ErrInvalidToken without consulting the DB")
	assert.Equal(t, 0, repo.getCallCount(),
		"a negative cache hit must short-circuit the DB lookup")
}

func TestService_VerifyAPIKey_PopulatesCacheOnSuccessfulMiss(t *testing.T) {
	rawToken := "rk_to_be_cached"
	keyHash := auth.HashAPIKeySHA256(rawToken)
	wantInstall := &auth.Installation{ID: "install_new", ExternalID: "org_new", Name: "fresh"}
	wantKey := &auth.APIKey{ID: "key_new", InstallationID: wantInstall.ID, ExternalID: wantInstall.ExternalID, KeyHash: keyHash}

	cache := newRecordingAPIKeyCache()
	svc, repo := makeServiceWithCacheAndCounter(t, cache, fakeKeyRow{apiKey: wantKey, installation: wantInstall})

	_, _, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.NoError(t, err)
	assert.Equal(t, 1, repo.getCallCount(),
		"a cache miss must consult the DB exactly once")

	sets := cache.setSnapshot()
	require.Len(t, sets, 1, "VerifyAPIKey must populate the cache after a successful DB lookup")
	assert.Equal(t, keyHash, sets[0].keyHash,
		"the cache key must be the SHA-256 hash of the bearer token")
	assert.False(t, sets[0].entry.Negative, "a successful lookup must populate a positive cache entry")
	require.NotNil(t, sets[0].entry.APIKey)
	assert.Equal(t, "key_new", sets[0].entry.APIKey.ID)
	require.NotNil(t, sets[0].entry.Installation)
	assert.Equal(t, "install_new", sets[0].entry.Installation.ID)
}

func TestService_VerifyAPIKey_PopulatesNegativeCacheOnNotFound(t *testing.T) {
	rawToken := "rk_definitely_not_in_db"
	keyHash := auth.HashAPIKeySHA256(rawToken)

	cache := newRecordingAPIKeyCache()
	svc, repo := makeServiceWithCacheAndCounter(t, cache)

	_, _, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.ErrorIs(t, err, auth.ErrInvalidToken)
	assert.Equal(t, 1, repo.getCallCount())

	sets := cache.setSnapshot()
	require.Len(t, sets, 1, "VerifyAPIKey must negative-cache an unknown token to defend the DB from credential stuffing")
	assert.Equal(t, keyHash, sets[0].keyHash)
	assert.True(t, sets[0].entry.Negative,
		"the cache entry for an unknown token must be marked Negative=true")
}

func TestService_VerifyAPIKey_DoesNotCacheTransportErrors(t *testing.T) {
	rawToken := "rk_db_will_error"
	cache := newRecordingAPIKeyCache()
	svc, repo := makeServiceWithCacheAndCounter(t, cache)
	repo.fakeAPIKeyRepository.override = errors.New("postgres connection refused")

	_, _, _, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.Error(t, err)
	assert.NotErrorIs(t, err, auth.ErrInvalidToken,
		"a transport error must not collapse into ErrInvalidToken")
	assert.Empty(t, cache.setSnapshot(),
		"transport errors must NOT populate the cache; the next request might succeed")
}

func makeServiceWithExternalKeys(t *testing.T, externalRepo auth.ExternalAPIKeyRepository, rows ...fakeKeyRow) *auth.Service {
	t.Helper()
	apiKeys := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}}
	for _, row := range rows {
		apiKeys.byHash[row.apiKey.KeyHash] = row
	}
	return auth.NewService(
		&fakeInstallationRepository{},
		apiKeys,
		externalRepo,
		nil,
		auth.NoOpAPIKeyCache{},
		nil,
		frozenClock(),
	)
}

func TestService_VerifyAPIKey_WithExternalKeys(t *testing.T) {
	rawToken := "rk_byok_test_token"
	keyHash := auth.HashAPIKeySHA256(rawToken)
	wantInstall := &auth.Installation{
		ID:         "install_byok",
		ExternalID: "org_byok",
		Name:       "byok-tenant",
	}
	wantKey := &auth.APIKey{
		ID:             "key_byok",
		InstallationID: wantInstall.ID,
		ExternalID:     wantInstall.ExternalID,
		KeyHash:        keyHash,
	}
	externalKey := &auth.ExternalAPIKey{
		ID:             "ext-key-1",
		InstallationID: wantInstall.ID,
		Provider:       "anthropic",
		Plaintext:      []byte("sk-ant-test-key"),
	}
	fakeExternal := &fakeExternalAPIKeyRepo{keys: []*auth.ExternalAPIKey{externalKey}}
	svc := makeServiceWithExternalKeys(t, fakeExternal, fakeKeyRow{apiKey: wantKey, installation: wantInstall})

	_, _, externalKeys, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.NoError(t, err)
	require.Len(t, externalKeys, 1,
		"VerifyAPIKey must return external keys fetched for the installation")
	assert.Equal(t, "anthropic", externalKeys[0].Provider)
	assert.Equal(t, []byte("sk-ant-test-key"), externalKeys[0].Plaintext)
}

func TestService_VerifyAPIKey_ExternalKeyErrorIsNonFatal(t *testing.T) {
	rawToken := "rk_extkey_error_test"
	keyHash := auth.HashAPIKeySHA256(rawToken)
	wantInstall := &auth.Installation{
		ID:         "install_extfail",
		ExternalID: "org_extfail",
		Name:       "extfail-tenant",
	}
	wantKey := &auth.APIKey{
		ID:             "key_extfail",
		InstallationID: wantInstall.ID,
		ExternalID:     wantInstall.ExternalID,
		KeyHash:        keyHash,
	}
	fakeExternal := &fakeExternalAPIKeyRepo{err: errors.New("external key repo unavailable")}
	svc := makeServiceWithExternalKeys(t, fakeExternal, fakeKeyRow{apiKey: wantKey, installation: wantInstall})

	gotInstall, gotKey, externalKeys, err := svc.VerifyAPIKey(context.Background(), rawToken)

	require.NoError(t, err,
		"an external key fetch error must not fail authentication")
	require.NotNil(t, gotInstall)
	require.NotNil(t, gotKey)
	assert.Nil(t, externalKeys,
		"when external key fetch fails, externalKeys must be nil rather than an error")
}

func TestService_VerifyAPIKey_ExternalKeysAreCached(t *testing.T) {
	rawToken := "rk_extkey_cache_test"
	keyHash := auth.HashAPIKeySHA256(rawToken)
	wantInstall := &auth.Installation{
		ID:         "install_extcache",
		ExternalID: "org_extcache",
		Name:       "extcache-tenant",
	}
	wantKey := &auth.APIKey{
		ID:             "key_extcache",
		InstallationID: wantInstall.ID,
		ExternalID:     wantInstall.ExternalID,
		KeyHash:        keyHash,
	}
	externalKey := &auth.ExternalAPIKey{
		ID:             "ext-cached-1",
		InstallationID: wantInstall.ID,
		Provider:       "openai",
		Plaintext:      []byte("sk-openai-test"),
	}
	fakeExternal := &fakeExternalAPIKeyRepo{keys: []*auth.ExternalAPIKey{externalKey}}

	cache := newRecordingAPIKeyCache()
	bare := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{
		keyHash: {apiKey: wantKey, installation: wantInstall},
	}}
	counter := &repoCallCounter{fakeAPIKeyRepository: bare}
	svc := auth.NewService(
		&fakeInstallationRepository{},
		counter,
		fakeExternal,
		nil,
		cache,
		nil,
		frozenClock(),
	)

	// First call: populates cache with external keys.
	_, _, externalKeys1, err := svc.VerifyAPIKey(context.Background(), rawToken)
	require.NoError(t, err)
	require.Len(t, externalKeys1, 1)

	// Second call: should hit the cache — DB must not be called again.
	_, _, externalKeys2, err := svc.VerifyAPIKey(context.Background(), rawToken)
	require.NoError(t, err)
	require.Len(t, externalKeys2, 1,
		"external keys must be returned on a cache hit")
	assert.Equal(t, 1, counter.getCallCount(),
		"the repo must only be called once; the second call must be served from cache")
}

func TestService_SetInstallationExcludedModels(t *testing.T) {
	installRepo := &fakeInstallationRepository{}
	svc := auth.NewService(installRepo, &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}}, nil, nil, auth.NoOpAPIKeyCache{}, nil, frozenClock())

	allowed := map[string]struct{}{"gpt-4o": {}, "claude-opus-4-7": {}}

	t.Run("persists deduped list scoped by external_id", func(t *testing.T) {
		out, err := svc.SetInstallationExcludedModels(context.Background(), "ext-1", "inst-1", []string{"gpt-4o", "gpt-4o", "claude-opus-4-7"}, allowed)
		require.NoError(t, err)
		assert.Equal(t, []string{"gpt-4o", "claude-opus-4-7"}, out, "duplicates collapsed; order preserved")
		assert.Equal(t, []string{"gpt-4o", "claude-opus-4-7"}, installRepo.excludedModelsByID["inst-1"])
		assert.Equal(t, "ext-1", installRepo.excludedModelsExternalByID["inst-1"],
			"external_id must be propagated to the repo for cross-tenant scoping")
	})

	t.Run("rejects unknown model with ErrUnknownModel", func(t *testing.T) {
		_, err := svc.SetInstallationExcludedModels(context.Background(), "ext-1", "inst-1", []string{"gemini-nope"}, allowed)
		require.Error(t, err)
		assert.True(t, errors.Is(err, auth.ErrUnknownModel))
	})

	t.Run("nil allowed skips validation", func(t *testing.T) {
		out, err := svc.SetInstallationExcludedModels(context.Background(), "ext-2", "inst-2", []string{"anything-goes"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"anything-goes"}, out)
	})

	t.Run("nil models persists empty slice", func(t *testing.T) {
		out, err := svc.SetInstallationExcludedModels(context.Background(), "ext-3", "inst-3", nil, allowed)
		require.NoError(t, err)
		assert.Equal(t, []string{}, out)
		assert.Equal(t, []string{}, installRepo.excludedModelsByID["inst-3"])
	})
}

type recordingNotifier struct {
	mu  sync.Mutex
	ids []string
}

func (n *recordingNotifier) NotifyInstallationChanged(installationID string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ids = append(n.ids, installationID)
}

func (n *recordingNotifier) snapshot() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.ids))
	copy(out, n.ids)
	return out
}

func TestService_WriteHooksInvalidateAndNotify(t *testing.T) {
	// Each per-installation write must drop the cached entries for that
	// installation AND publish a NOTIFY so peer replicas do the same. The
	// 5-min TTL is the safety net, not the steady-state behavior — without
	// these hooks the dashboard's "save" button is a lie for up to 5 minutes.
	const installID = "inst-A"

	makeSvc := func() (*auth.Service, *recordingAPIKeyCache, *recordingNotifier) {
		cache := newRecordingAPIKeyCache()
		// Pre-populate so InvalidateInstallation has something to drop.
		cache.Set("hash-A", auth.CachedKey{
			APIKey:       &auth.APIKey{ID: "k1"},
			Installation: &auth.Installation{ID: installID},
		})
		nf := &recordingNotifier{}
		installRepo := &fakeInstallationRepository{}
		extRepo := &fakeExternalAPIKeyRepo{}
		apiKeyRepo := &fakeAPIKeyRepository{byHash: map[string]fakeKeyRow{}}
		svc := auth.NewService(installRepo, apiKeyRepo, extRepo, nil, cache, nil, frozenClock()).
			WithInstallationChangeNotifier(nf)
		return svc, cache, nf
	}

	t.Run("SetInstallationExcludedModels", func(t *testing.T) {
		svc, cache, nf := makeSvc()
		_, err := svc.SetInstallationExcludedModels(context.Background(), "ext-1", installID, []string{"gpt-4o"}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{installID}, cache.invalidationSnapshot(),
			"excluded-model writes must call cache.InvalidateInstallation so the next request sees the new list")
		assert.Equal(t, []string{installID}, nf.snapshot(),
			"excluded-model writes must publish NOTIFY so peer replicas drop their cache too")
	})

	t.Run("UpsertExternalAPIKey", func(t *testing.T) {
		svc, cache, nf := makeSvc()
		_, err := svc.UpsertExternalAPIKey(context.Background(), installID, "anthropic", "sk-abc", nil, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{installID}, cache.invalidationSnapshot(),
			"BYOK upsert must drop the cached ExternalKeys so the new credential is picked up on the next request, not in 5 minutes")
		assert.Equal(t, []string{installID}, nf.snapshot())
	})

	t.Run("DeleteExternalAPIKey", func(t *testing.T) {
		svc, cache, nf := makeSvc()
		err := svc.DeleteExternalAPIKey(context.Background(), installID, "ekid-1")
		require.NoError(t, err)
		assert.Equal(t, []string{installID}, cache.invalidationSnapshot())
		assert.Equal(t, []string{installID}, nf.snapshot())
	})
}
