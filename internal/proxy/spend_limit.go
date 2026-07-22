package proxy

import (
	"context"
	"fmt"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
)

// checkUserMonthlySpendLimit gates a turn on the resolved engineer's monthly
// spend limit. Runs inside the proxy (not middleware) because user identity is
// resolved by the handler after the middleware chain; no identity passes through.
//
// The cap bounds PAID spend, not free subscription usage: a covered turn on a
// usage-bypass org (a Claude/Codex credential that covers routePath) is not
// rejected when the cap is reached. Instead it returns a subscription-only
// ctx (billing.WithSubscriptionOnly) so the proxy serves it on the caller's
// own plan — or refuses a would-be-paid turn with the controlled 402 — never
// on a paid model. Mirrors middleware.WithBalanceCheck's exemption. Returns the
// (possibly flagged) ctx the rest of the turn must use.
func (s *Service) checkUserMonthlySpendLimit(ctx context.Context, headers http.Header, routePath string) (context.Context, error) {
	if s.billing == nil {
		return ctx, nil
	}
	// Billing override is the org-wide escape hatch (WithBalanceCheck stamps it
	// and passes those orgs through), so engineer limits don't apply either.
	if billing.HasOverrideFromContext(ctx) {
		return ctx, nil
	}
	userID := auth.UserIDFrom(ctx)
	if userID == "" {
		return ctx, nil
	}
	orgID, _ := ctx.Value(ExternalIDContextKey{}).(string)
	if orgID == "" {
		return ctx, nil
	}
	result, err := s.billing.CheckUserMonthlySpend(ctx, orgID, userID)
	if err != nil {
		observability.FromContext(ctx).Error("User monthly spend-limit check failed; refusing request",
			"err", err, "organization_id", orgID, "router_user_id", userID)
		return ctx, fmt.Errorf("%w: %v", billing.ErrSpendLimitCheckUnavailable, err)
	}
	if result.LimitReached() {
		// Cap reached but the caller's own subscription covers this route: don't
		// 402 free traffic. Flag subscription-only so the proxy serves on the
		// subscription (or refuses a would-be-paid turn), bounding paid spend at
		// the cap while subscription usage keeps flowing.
		if _, bypassOn := usageBypassFromContext(ctx); bypassOn &&
			RequestPresentsCoveringSubscription(ctx, headers, routePath) {
			observability.FromContext(ctx).Info("Engineer monthly cap reached but subscription covers the route: serving subscription-only",
				"organization_id", orgID,
				"router_user_id", userID,
				"spent_usd_micros", result.SpentMicros,
				"monthly_limit_usd_micros", *result.LimitMicros,
			)
			return billing.WithSubscriptionOnly(ctx), nil
		}
		observability.FromContext(ctx).Info("Request rejected: engineer monthly spend limit reached",
			"organization_id", orgID,
			"router_user_id", userID,
			"spent_usd_micros", result.SpentMicros,
			"monthly_limit_usd_micros", *result.LimitMicros,
		)
		return ctx, fmt.Errorf("%w: spent %d of %d usd micros", billing.ErrUserMonthlySpendLimitReached, result.SpentMicros, *result.LimitMicros)
	}
	return ctx, nil
}
