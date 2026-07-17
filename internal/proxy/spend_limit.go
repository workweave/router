package proxy

import (
	"context"
	"fmt"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
)

// checkUserMonthlySpendLimit gates a turn on the resolved engineer's monthly
// spend limit. Runs inside the proxy (not middleware) because user identity is
// resolved by the handler after the middleware chain; no identity passes through.
func (s *Service) checkUserMonthlySpendLimit(ctx context.Context) error {
	if s.billing == nil {
		return nil
	}
	// Billing override is the org-wide escape hatch (WithBalanceCheck stamps it
	// and passes those orgs through), so engineer limits don't apply either.
	if billing.HasOverrideFromContext(ctx) {
		return nil
	}
	userID := auth.UserIDFrom(ctx)
	if userID == "" {
		return nil
	}
	orgID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	if orgID == "" {
		return nil
	}
	result, err := s.billing.CheckUserMonthlySpend(ctx, orgID, userID)
	if err != nil {
		observability.FromContext(ctx).Error("User monthly spend-limit check failed; refusing request",
			"err", err, "organization_id", orgID, "router_user_id", userID)
		return fmt.Errorf("%w: %v", billing.ErrSpendLimitCheckUnavailable, err)
	}
	if result.LimitReached() {
		observability.FromContext(ctx).Info("Request rejected: engineer monthly spend limit reached",
			"organization_id", orgID,
			"router_user_id", userID,
			"spent_usd_micros", result.SpentMicros,
			"monthly_limit_usd_micros", *result.LimitMicros,
		)
		return fmt.Errorf("%w: spent %d of %d usd micros", billing.ErrUserMonthlySpendLimitReached, result.SpentMicros, *result.LimitMicros)
	}
	return nil
}
