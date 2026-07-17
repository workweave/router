package billing_test

import (
	"context"
	"errors"
	"testing"

	"workweave/router/internal/billing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func limitPtr(v int64) *int64 { return &v }

func TestMonthlySpendResult_LimitReached(t *testing.T) {
	assert.False(t, billing.MonthlySpendResult{SpentMicros: 999_999_999}.LimitReached(), "nil limit is never reached")
	assert.False(t, billing.MonthlySpendResult{SpentMicros: 999_999, LimitMicros: limitPtr(1_000_000)}.LimitReached())
	assert.True(t, billing.MonthlySpendResult{SpentMicros: 1_000_000, LimitMicros: limitPtr(1_000_000)}.LimitReached())
	assert.True(t, billing.MonthlySpendResult{SpentMicros: 2_500_000, LimitMicros: limitPtr(1_000_000)}.LimitReached())
}

func TestCheckUserMonthlySpend(t *testing.T) {
	repo := &fakeRepo{userMonthSpent: 42, userMonthLimit: limitPtr(100)}
	svc := billing.NewService(repo)
	res, err := svc.CheckUserMonthlySpend(context.Background(), "org-1", "user-1")
	require.NoError(t, err)
	assert.Equal(t, int64(42), res.SpentMicros)
	require.NotNil(t, res.LimitMicros)
	assert.Equal(t, int64(100), *res.LimitMicros)
}

func TestCheckUserMonthlySpend_RepoError(t *testing.T) {
	sentinel := errors.New("pg down")
	svc := billing.NewService(&fakeRepo{userMonthErr: sentinel})
	_, err := svc.CheckUserMonthlySpend(context.Background(), "org-1", "user-1")
	assert.ErrorIs(t, err, sentinel)
}

func TestCheckOrgMonthlySpend(t *testing.T) {
	repo := &fakeRepo{orgMonthSpent: 7, orgMonthLimit: nil}
	svc := billing.NewService(repo)
	res, err := svc.CheckOrgMonthlySpend(context.Background(), "org-1")
	require.NoError(t, err)
	assert.Equal(t, int64(7), res.SpentMicros)
	assert.Nil(t, res.LimitMicros)
}

func TestCheckOrgMonthlySpend_RepoError(t *testing.T) {
	sentinel := errors.New("pg down")
	svc := billing.NewService(&fakeRepo{orgMonthErr: sentinel})
	_, err := svc.CheckOrgMonthlySpend(context.Background(), "org-1")
	assert.ErrorIs(t, err, sentinel)
}
