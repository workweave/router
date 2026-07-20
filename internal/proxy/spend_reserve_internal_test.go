package proxy

import (
	"context"
	"sync/atomic"
	"testing"

	"workweave/router/internal/billing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// armCountingRepo records ReserveSpendCaps calls so tests can assert the
// agent-shadow path never reaches billing reservation.
type armCountingRepo struct {
	spendLimitRepo
	reserveCalls atomic.Int64
	reserveErr   error
}

func (r *armCountingRepo) ReserveSpendCaps(context.Context, billing.ReserveSpendCapsParams) ([]uuid.UUID, error) {
	r.reserveCalls.Add(1)
	if r.reserveErr != nil {
		return nil, r.reserveErr
	}
	return []uuid.UUID{uuid.New()}, nil
}

func agentShadowCtx(orgID string) context.Context {
	ctx := spendLimitCtx("u1", orgID)
	ctx = context.WithValue(ctx, APIKeyIDContextKey{}, "key-1")
	return context.WithValue(ctx, AgentShadowEvalContextKey{}, AgentShadowEvaluation{
		Model:     "claude-opus-4-8",
		RolloutID: "pilot-1",
		StateID:   "state-1",
	})
}

func TestArmSpendReservations_AgentShadowEvalSkipsReserve(t *testing.T) {
	// Repo would refuse a real reserve (tight/exceeded limit). Shadow eval must
	// still pass with zero reservations and never call ReserveSpendCaps.
	repo := &armCountingRepo{reserveErr: billing.ErrOrgMonthlySpendLimitReached}
	s := &Service{billing: billing.NewService(repo)}

	ctx, release, err := s.ArmSpendReservations(agentShadowCtx("org-1"))
	require.NoError(t, err)
	require.NotNil(t, release)
	assert.Equal(t, int64(0), repo.reserveCalls.Load(), "shadow eval must not call ReserveSpendCaps")
	assert.Nil(t, billing.SpendHoldFrom(ctx), "shadow eval must leave ctx unarmed")
	assert.NotPanics(t, release)
}

func TestArmSpendReservations_NonShadowStillReserves(t *testing.T) {
	repo := &armCountingRepo{}
	s := &Service{billing: billing.NewService(repo)}

	ctx, release, err := s.ArmSpendReservations(spendLimitCtx("u1", "org-1"))
	require.NoError(t, err)
	require.NotNil(t, release)
	assert.Equal(t, int64(1), repo.reserveCalls.Load())
	assert.NotNil(t, billing.SpendHoldFrom(ctx))
	release()
}

func TestArmSpendReservations_NonShadowPropagatesCapError(t *testing.T) {
	repo := &armCountingRepo{reserveErr: billing.ErrOrgMonthlySpendLimitReached}
	s := &Service{billing: billing.NewService(repo)}

	_, release, err := s.ArmSpendReservations(spendLimitCtx("u1", "org-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrOrgMonthlySpendLimitReached)
	assert.Nil(t, release)
	assert.Equal(t, int64(1), repo.reserveCalls.Load())
}
