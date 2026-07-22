package proxy

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"

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
func (r *spendLimitRepo) GetAPIKeySpend(context.Context, string) (int64, *int64, bool, error) {
	return 0, nil, false, nil
}
func (r *spendLimitRepo) GetUserMonthlySpendAndLimit(context.Context, string, string) (int64, *int64, error) {
	return r.spent, r.limit, r.err
}
func (r *spendLimitRepo) GetOrgMonthlySpendAndLimit(context.Context, string) (int64, *int64, error) {
	return 0, nil, nil
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

// spendLimitCheck drives checkUserMonthlySpendLimit with no header/route (the
// non-subscription paths never consult them), returning just the error.
func spendLimitCheck(s *Service, ctx context.Context) error {
	_, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, "")
	return err
}

// withUsageBypassSubscription layers a usage-bypass config plus a valid Claude
// subscription token onto ctx, exactly as the auth middleware would.
func withUsageBypassSubscription(ctx context.Context, token string) context.Context {
	ctx = context.WithValue(ctx, InstallationUsageBypassContextKey{}, UsageBypassConfig{Enabled: true})
	return context.WithValue(ctx, AnthropicSubscriptionContextKey{}, token)
}

func TestCheckUserMonthlySpendLimit_NoBillingSkips(t *testing.T) {
	s := &Service{}
	assert.NoError(t, spendLimitCheck(s, spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_NoIdentitySkips(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 10, limit: micros(1), err: errors.New("must not be called")})}
	assert.NoError(t, spendLimitCheck(s, spendLimitCtx("", "org-1")),
		"a request with no resolvable user identity passes through")
}

func TestCheckUserMonthlySpendLimit_NoLimitPasses(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 999_999_999, limit: nil})}
	assert.NoError(t, spendLimitCheck(s, spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_UnderLimitPasses(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 999_999, limit: micros(1_000_000)})}
	assert.NoError(t, spendLimitCheck(s, spendLimitCtx("u1", "org-1")))
}

func TestCheckUserMonthlySpendLimit_AtLimitRejected(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	err := spendLimitCheck(s, spendLimitCtx("u1", "org-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
}

func TestCheckUserMonthlySpendLimit_ReadErrorFailsClosed(t *testing.T) {
	s := &Service{billing: billing.NewService(&spendLimitRepo{err: errors.New("pg down")})}
	err := spendLimitCheck(s, spendLimitCtx("u1", "org-1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrSpendLimitCheckUnavailable)
}

func TestCheckUserMonthlySpendLimit_OverrideSkips(t *testing.T) {
	// Even over the limit (and with a repo that would error), a billing-override
	// request bypasses engineer enforcement — WithBalanceCheck already let it in.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000), err: errors.New("must not be called")})}
	ctx := context.WithValue(spendLimitCtx("u1", "org-1"), billing.HasOverrideContextKey, true)
	assert.NoError(t, spendLimitCheck(s, ctx))
}

func TestCheckUserMonthlySpendLimit_CapReachedCoveringSubscriptionServesSubscriptionOnly(t *testing.T) {
	// The fix: cap reached, but a usage-bypass org presents a Claude subscription
	// that covers /v1/messages. Don't 402 — flag the returned ctx subscription-only
	// so the proxy serves free on the caller's own plan. Paid spend stays capped.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	ctx := withUsageBypassSubscription(spendLimitCtx("u1", "org-1"), "sk-ant-oat01-valid-token")
	outCtx, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, routePathMessages)
	require.NoError(t, err, "a covered turn must not 402 when the cap is reached")
	assert.True(t, billing.SubscriptionOnlyFromContext(outCtx), "the returned ctx must carry the subscription-only flag")
}

func TestCheckUserMonthlySpendLimit_CapReachedNoSubscriptionStillRejects(t *testing.T) {
	// Usage-bypass on, but no subscription credential present: the turn would route
	// to a paid model, so a reached cap must still 402.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	ctx := context.WithValue(spendLimitCtx("u1", "org-1"), InstallationUsageBypassContextKey{}, UsageBypassConfig{Enabled: true})
	_, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, routePathMessages)
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
}

func TestCheckUserMonthlySpendLimit_CapReachedSubscriptionWithoutBypassRejects(t *testing.T) {
	// Subscription present, but the org has NOT enabled usage-bypass. The exemption
	// is scoped to bypass orgs, so a reached cap must still 402.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	ctx := context.WithValue(spendLimitCtx("u1", "org-1"), AnthropicSubscriptionContextKey{}, "sk-ant-oat01-valid-token")
	_, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, routePathMessages)
	require.Error(t, err)
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
}

func TestCheckUserMonthlySpendLimit_CapReachedSubscriptionUncoveredRouteRejects(t *testing.T) {
	// A Claude subscription can't serve the OpenAI chat route, so the exemption is
	// route-scoped: a reached cap on an uncovered route must still 402.
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 1_000_000, limit: micros(1_000_000)})}
	ctx := withUsageBypassSubscription(spendLimitCtx("u1", "org-1"), "sk-ant-oat01-valid-token")
	_, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, routePathChatCompletions)
	require.Error(t, err, "a Claude sub must not exempt an OpenAI-route request")
	assert.ErrorIs(t, err, billing.ErrUserMonthlySpendLimitReached)
}

func TestCheckUserMonthlySpendLimit_UnderLimitDoesNotFlagSubscriptionOnly(t *testing.T) {
	// Below the cap, a subscription request routes normally: the returned ctx must
	// NOT be flagged subscription-only (that flag disables paid failover).
	s := &Service{billing: billing.NewService(&spendLimitRepo{spent: 500_000, limit: micros(1_000_000)})}
	ctx := withUsageBypassSubscription(spendLimitCtx("u1", "org-1"), "sk-ant-oat01-valid-token")
	outCtx, err := s.checkUserMonthlySpendLimit(ctx, http.Header{}, routePathMessages)
	require.NoError(t, err)
	assert.False(t, billing.SubscriptionOnlyFromContext(outCtx), "an under-cap turn must not be forced subscription-only")
}
