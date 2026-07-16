package subscriptions

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"workweave/router/internal/providers"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	waitShort = 2 * time.Second
	pollShort = 5 * time.Millisecond

	testInstallation = "11111111-1111-1111-1111-111111111111"
	testEmail        = "dev@example.com"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRepo is an in-memory subscriptions.Repository for unit tests.
type fakeRepo struct {
	mu          sync.Mutex
	byID        map[string]*Credential
	updateCalls int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{byID: make(map[string]*Credential)}
}

func (r *fakeRepo) put(c *Credential) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[c.ID] = c
}

func (r *fakeRepo) Create(_ context.Context, p CreateParams) (*Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cred := &Credential{
		ID:             "cred-" + p.ExternalID,
		ExternalID:     p.ExternalID,
		InstallationID: p.InstallationID,
		UserEmail:      p.UserEmail,
		Provider:       p.Provider,
		AccountLabel:   p.AccountLabel,
		AccessToken:    p.AccessToken,
		RefreshToken:   p.RefreshToken,
		ExpiresAt:      p.ExpiresAt,
		CreatedAt:      time.Unix(1_000_000, 0),
	}
	r.byID[cred.ID] = cred
	return cred, nil
}

func (r *fakeRepo) GetActiveForUser(_ context.Context, installationID, userEmail string) ([]*Credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*Credential
	for _, c := range r.byID {
		if c.InstallationID == installationID && c.UserEmail == userEmail && c.RefreshFailedAt.IsZero() {
			out = append(out, c)
		}
	}
	return out, nil
}

func (r *fakeRepo) ListForUser(ctx context.Context, installationID, userEmail string) ([]*Credential, error) {
	return r.GetActiveForUser(ctx, installationID, userEmail)
}

func (r *fakeRepo) UpdateTokens(_ context.Context, id, _, _ string, access, refresh []byte, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updateCalls++
	if c := r.byID[id]; c != nil {
		c.AccessToken = access
		c.RefreshToken = refresh
		c.ExpiresAt = expiresAt
	}
	return nil
}

func (r *fakeRepo) MarkRefreshFailed(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.byID[id]; c != nil {
		c.RefreshFailedAt = time.Unix(1_000_500, 0)
	}
	return nil
}

func (r *fakeRepo) MarkUsed(context.Context, string) error { return nil }

func (r *fakeRepo) SoftDelete(_ context.Context, installationID, userEmail, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.byID[id]
	if c == nil || c.InstallationID != installationID || c.UserEmail != userEmail {
		return false, nil
	}
	delete(r.byID, id)
	return true, nil
}

func (r *fakeRepo) SoftDeleteByFingerprint(context.Context, string, string, string, string) error {
	return nil
}

func seedExpiredCredential(r *fakeRepo) *Credential {
	cred := &Credential{
		ID:             "cred-expired",
		ExternalID:     "scid_expired",
		InstallationID: testInstallation,
		UserEmail:      testEmail,
		Provider:       providers.ProviderAnthropic,
		AccessToken:    []byte("sk-ant-oat01-stale"),
		RefreshToken:   []byte("refresh-old"),
		ExpiresAt:      time.Unix(1, 0), // long past
	}
	r.put(cred)
	return cred
}

func TestService_SelectSkipsAndReturnsFirstUsable(t *testing.T) {
	repo := newFakeRepo()
	c1 := &Credential{ID: "c1", InstallationID: testInstallation, UserEmail: testEmail, Provider: providers.ProviderAnthropic, AccessToken: []byte("t1")}
	c2 := &Credential{ID: "c2", InstallationID: testInstallation, UserEmail: testEmail, Provider: providers.ProviderAnthropic, AccessToken: []byte("t2")}
	repo.put(c1)
	repo.put(c2)
	svc := NewService(repo, &fakeRefresher{}, testLogger())

	got, err := svc.SelectCredential(context.Background(), testInstallation, testEmail, providers.ProviderAnthropic, func(id string) bool {
		return id == "c1"
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEqual(t, "c1", got.ID, "the vetoed credential must be skipped")
}

func TestService_SelectRefreshesExpiring(t *testing.T) {
	repo := newFakeRepo()
	seedExpiredCredential(repo)
	svc := NewService(repo, &fakeRefresher{}, testLogger())

	got, err := svc.SelectCredential(context.Background(), testInstallation, testEmail, providers.ProviderAnthropic, nil)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []byte("fresh"), got.AccessToken, "an expiring credential must be refreshed before use")
	assert.Equal(t, 1, repo.updateCalls, "rotated tokens must be persisted")
}

func TestService_PoolExistsAndRemove(t *testing.T) {
	repo := newFakeRepo()
	c := &Credential{ID: "c1", InstallationID: testInstallation, UserEmail: testEmail, Provider: providers.ProviderAnthropic, AccessToken: []byte("t1")}
	repo.put(c)
	svc := NewService(repo, &fakeRefresher{}, testLogger())

	assert.True(t, svc.PoolExists(context.Background(), testInstallation, testEmail, providers.ProviderAnthropic))
	assert.False(t, svc.PoolExists(context.Background(), testInstallation, testEmail, providers.ProviderOpenAI))

	require.NoError(t, svc.Remove(context.Background(), testInstallation, testEmail, "c1"))
	err := svc.Remove(context.Background(), testInstallation, testEmail, "c1")
	assert.ErrorIs(t, err, ErrCredentialNotFound, "removing a gone credential reports not-found")
}
