package middleware

import (
	"context"
	"errors"
	"net/http"

	"workweave/router/internal/billing"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

// ctxKeyHasBillingOverride is the gin context key used to plumb the
// override flag from this middleware to the proxy's debit hook.
const ctxKeyHasBillingOverride = "router_has_billing_override"

// TopUpURL is the customer-facing page where org admins buy credits.
// Returned in the 402 body so the client can surface a CTA.
const TopUpURL = "https://app.workweave.ai/settings/billing/router-credits"

// WithBalanceCheck enforces prepaid credit gating on inference routes.
// Attached only in managed mode and only after WithAuth, so the
// installation lookup below is guaranteed to be populated.
//
// Behavior:
//   - Override row present → pass through; flag the request context so
//     the proxy's debit hook writes a delta=0 ledger row.
//   - Balance ≤ minBalanceMicros → HTTP 402 with structured JSON body.
//   - Otherwise → pass through.
//
// The balance read is a single indexed SELECT (~2-5ms in-region). Any
// repo error is logged and fail-open: the customer's request continues
// rather than 500ing. The plan accepts the rare debit-failure window;
// repeated infra failures will surface in metrics.
func WithBalanceCheck(svc *billing.Service, minBalanceMicros int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		installation := InstallationFrom(c)
		if installation == nil || installation.ExternalID == "" {
			// Should never happen: WithAuth runs first and would have
			// 401'd. Log Debug rather than Error so a synthetic request
			// missing the auth setup doesn't page on-call.
			log.Debug("Balance check skipped: no installation on request context")
			c.Next()
			return
		}

		orgID := installation.ExternalID
		result, err := svc.CheckBalance(c.Request.Context(), orgID)
		if err != nil {
			if errors.Is(err, billing.ErrBalanceRowMissing) {
				log.Info("Balance check rejected: balance row missing", "organization_id", orgID)
				c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
					"error":              "insufficient_credits",
					"top_up_url":         TopUpURL,
					"balance_usd_micros": 0,
					"message":            "Your organization's prepaid credits are depleted. Contact your org admin to add credits.",
				})
				return
			}
			// Infra error reading billing tables. Fail open so we don't
			// blackhole inference on a transient DB hiccup; the debit
			// hook will likely also fail and surface in metrics.
			log.Error("Balance check failed; allowing request to proceed", "err", err, "organization_id", orgID)
			c.Next()
			return
		}

		if result.HasOverride {
			c.Set(ctxKeyHasBillingOverride, true)
			ctx := context.WithValue(c.Request.Context(), billing.HasOverrideContextKey, true)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}

		if result.BalanceMicros <= minBalanceMicros {
			log.Info("Balance check rejected: balance at or below threshold",
				"organization_id", orgID,
				"balance_usd_micros", result.BalanceMicros,
				"threshold_usd_micros", minBalanceMicros,
			)
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error":              "insufficient_credits",
				"top_up_url":         TopUpURL,
				"balance_usd_micros": result.BalanceMicros,
				"message":            "Your organization's prepaid credits are depleted. Contact your org admin to add credits.",
			})
			return
		}

		c.Next()
	}
}
