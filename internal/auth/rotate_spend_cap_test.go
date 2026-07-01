package auth_test

// TestRotateAPIKey_preservesSpendCap is a FAILING test that documents the bug:
// RotateAPIKey issues the replacement key via IssueAPIKey, which calls Create
// without forwarding spend_cap_usd_micros. The new key always gets a nil cap,
// silently removing any budget limit that was set on the original key.
//
// Fix: propagate SpendCapUsdMicros through CreateAPIKeyParams so RotateAPIKey
// can carry the old key's cap onto the new one.

import (
	"context"
	"testing"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rotateCapKeyRepo is a minimal APIKeyRepository that supports the Create and
// ListForInstallation calls that RotateAPIKey drives. Create stores the new key
// keyed by ID; ListForInstallation returns whichever keys belong to that
// installation. SoftDelete marks a key deleted but leaves it visible only to
// ListForInstallation (so RotateAPIKey can find the old key to copy from).
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

	// Seed a key that already has a spend cap (simulates a cap set out-of-band
	// by the control plane, e.g. direct DB write).
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

	// Rotate the capped key.
	newKey, _, err := svc.RotateAPIKey(context.Background(), installationID, originalKeyID, nil)
	require.NoError(t, err)

	// The replacement key MUST carry the same spend cap so the budget is not silently erased.
	//
	// This assertion FAILS with the current implementation because IssueAPIKey →
	// Create is called without SpendCapUsdMicros, so the new key always gets nil.
	require.NotNil(t, newKey.SpendCapUsdMicros,
		"spend cap must be carried forward to the replacement key; got nil (cap was %d micros on old key)", cap)
	assert.Equal(t, cap, *newKey.SpendCapUsdMicros,
		"replacement key's spend cap must equal the original key's cap")
}
