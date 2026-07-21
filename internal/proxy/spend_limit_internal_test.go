package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spendLimitRepo is the minimum billing.Repo surface needed to drive
// checkUserMonthlySpendLimit through every branch.
type spendLimitRepo struct {
	spent int64
	limit *int64
	err   error
}

func (r *spendLimitRepo) GetBalance(context.Context, string) (int64, error) { return 0, nil }
func (r *spendLimitRepo) HasActiveOverride(context.Context, string) (bool, error) {
	return false, nil
}
func (r *spendLimitRepo) DebitInference(context.Context, billing.DebitParams) (int64, error) {
	return 0, nil
}
func (r *spendLimitRepo) GetAPIKeySpend(context.Context, string) (int64, int64, *int64, bool, error) {
	return 0, 0, nil, false, nil
}
func (r *spendLimitRepo) GetUserMonthlySpendAndLimit(context.Context, string, string) (int64, int64, *int64, error) {
	return r.spent, 0, r.limit, r.err
}
func (r *spendLimitRepo) GetOrgMonthlySpendAndLimit(context.Context, string) (int64, int64, *int64, error) {
	return 0, 0, nil, nil
}
func (r *spendLimitRepo) ReserveSpendCaps(context.Context, billing.ReserveSpendCapsParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (r *spendLimitRepo) ReleaseSpendReservations(context.Context, []uuid.UUID) error { return nil }
func (r *spendLimitRepo) SweepExpiredSpendReservations(context.Context, time.Time) (int, error) {
	return 0, nil
}
func (r *spendLimitRepo) GetAutopayConfig(context.Context, string) (bool, int64, error) {
	return false, 0, nil
}
func (r *spendLimitRepo) BillingTablesExist(context.Context) (bool, error) { return true, nil }

func spendLimitCtx(userID, orgID string) context.Context {
	ctx := context.Background()
	if userID != "" {
		ctx = context.WithValue(ctx, auth.UserIDContextKey{}, userID)
	}
	if orgID != "" {
		ctx = context.WithValue(ctx, ExternalIDContextKey{}, orgID)
	}
	return ctx
}

func micros(v int64) *int64 { return &v }

func TestCheckUserMonthlySpendLimit_NoBillingSkips(t *testing.T) {
	s := &Service{}
	assert.NoError(t, s.checkUserMonthlySpendLimit(spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_NoIdentitySkips(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 10, limit: micros(1), err: errors.New("must not be called")})}
	assert.NoError(t, s.checkUserMonthlySpendLimit(spendLimitCtx("", "org-1")),
		"a request with no resolvable user identity passes through")
}

func TestCheckUserMonthlySpendLimit_NoLimitPasses(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 999_999_999, limit: nil})}
	assert.NoError(t, s.checkUserMonthlySpendLimit(spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_UnderLimitPasses(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 999_999, limit: micros(1_000_000)})}
	assert.NoError(t, s.checkUserMonthlySpendLimit(spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_AtLimitRejected(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	err := s.checkUserMonthlySpendLimit(spendLimitCtx("u1", "org-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
}

func TestCheckUserMonthlySpendLimit_ReadErrorFailsClosed(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{err: errors.New("pg down")})}
	err := s.checkUserMonthlySpendLimit(spendLimitCtx("u1", "org-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrSpendLimitCheckUnavailable)
}

func TestCheckUserMonthlySpendLimit_OverrideSkips(t *testing.T) {
	// Even over the limit (and with a repo that would error), a billing-override
	// request bypasses engineer enforcement — WithBalanceCheck already let it in.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000), err: errors.New("must not be called")})}
	ctx := context.WithValue(spendLimitCtx("u1", "org-1"), billing.HasOverrideContextKey, true)
	assert.NoError(t, s.checkUserMonthlySpendLimit(ctx))
}
