package auth_test

import (
	"context"
	"testing"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type rotateCapKeyRepo struct {
	keys []*auth.APIKey
	next int
}

func (r *rotateCapKeyRepo) Create(_ context.Context, p auth.CreateAPIKeyParams) (*auth.APIKey, error) {
	r.next++
	key := &auth.APIKey{
		ID:                auth.GenerateID("kid"),
		InstallationID:    p.InstallationID,
		ExternalID:        p.ExternalID,
		Name:              p.Name,
		KeyPrefix:         p.KeyPrefix,
		KeyHash:           p.KeyHash,
		KeySuffix:         p.KeySuffix,
		CreatedBy:         p.CreatedBy,
		SpendCapUsdMicros: p.SpendCapUsdMicros,
	}
	r.keys = append(r.keys, key)
	return key, nil
}

func (r *rotateCapKeyRepo) GetActiveByHashWithInstallation(_ context.Context, _ string) (*auth.APIKey, *auth.Installation, error) {
	return nil, nil, nil
}

func (r *rotateCapKeyRepo) ListForInstallation(_ context.Context, installationID string) ([]*auth.APIKey, error) {
	var out []*auth.APIKey
	for _, k := range r.keys {
		if k.InstallationID == installationID && k.DeletedAt == nil {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *rotateCapKeyRepo) MarkUsed(_ context.Context, _ string) error { return nil }

func (r *rotateCapKeyRepo) SoftDelete(_ context.Context, installationID, id string) error {
	for _, k := range r.keys {
		if k.InstallationID == installationID && k.ID == id {
			now := frozenClock()()
			k.DeletedAt = &now
		}
	}
	return nil
}

func TestRotateAPIKey_preservesSpendCap(t *testing.T) {
	const installationID = "00000000-0000-0000-0000-000000000001"
	cap := int64(5_000_000) // $5.00 in micros

	repo := &rotateCapKeyRepo{}
	svc := auth.NewService(
		&fakeInstallationRepository{},
		repo,
		nil, // externalKeys — unused
		nil, // users — unused
		auth.NoOpAPIKeyCache{},
		nil, // userCache — unused
		frozenClock(),
	)

	originalKeyID := auth.GenerateID("kid")
	originalName := "budget-key"
	original := &auth.APIKey{
		ID:                originalKeyID,
		InstallationID:    installationID,
		ExternalID:        auth.GenerateID("ekid"),
		Name:              &originalName,
		KeyHash:           "hash-of-original",
		KeyPrefix:         "rk_or",
		KeySuffix:         "igin",
		SpendCapUsdMicros: &cap,
	}
	repo.keys = append(repo.keys, original)

	newKey, _, err := svc.RotateAPIKey(context.Background(), installationID, originalKeyID, nil)
	require.NoError(t, err)

	require.NotNil(t, newKey.SpendCapUsdMicros, "spend cap must carry forward to the replacement key")
	assert.Equal(t, cap, *newKey.SpendCapUsdMicros)
}
