package middleware

import (
	"context"
	"errors"
	"net/http"

	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// TopUpURL is the customer-facing page where org admins buy credits.
// Returned in the 402 body so the client can surface a CTA.
const TopUpURL = "https://app.workweave.ai/settings/billing/router-credits"

// WithBalanceCheck enforces prepaid credit gating on inference routes.
// Attached only in managed mode and only after WithAuth, so the
// installation lookup below is guaranteed to be populated.
//
// Behavior (evaluated in order):
//   - Override row present → pass through; flag the request context so
//     the proxy's debit hook writes a delta=0 ledger row.
//   - Balance ≤ minBalanceMicros (or no balance row) → HTTP 402. A
//     subscription-exempt request (UsageBypassEnabled + a validated Claude/Codex
//     cred that covers this route) instead gates at a negative overdraft floor,
//     so free traffic keeps flowing while any paid failover is bounded. Override
//     detection above still runs.
//   - Otherwise → pass through.
//
// The balance read is a single indexed SELECT (~2-5ms in-region). Any
// repo error fails closed with HTTP 503: in a prepaid credit system,
// allowing requests through when the gate cannot read the balance
// creates an unbilled-usage window where platform spend is incurred
// against an unknown balance. A short retry window for clients is the
// correct tradeoff vs. silently letting tenants spend without billing.
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

		// Subscription turns debit $0, so gating them on prepaid credits blocks
		// free traffic. Covers only the route's matching family (Codex can't serve
		// /v1/messages) and is applied only to 402 paths below — CheckBalance still
		// runs so an active override is detected and its context flag set.
		subscriptionExempt := installation.UsageBypassEnabled &&
			proxy.RequestPresentsCoveringSubscription(c.Request.Context(), c.Request.Header, c.FullPath())

		result, err := svc.CheckBalance(c.Request.Context(), orgID)
		if err != nil {
			if errors.Is(err, billing.ErrBalanceRowMissing) {
				// A subscription usage-bypass org may never have had a balance
				// row; its turns are free, so exempt them here too.
				if subscriptionExempt {
					log.Debug("Balance check skipped: subscription usage-bypass request, no balance row",
						"organization_id", orgID)
					c.Next()
					return
				}
				log.Info("Balance check rejected: balance row missing", "organization_id", orgID)
				c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
					"error":              "insufficient_credits",
					"top_up_url":         TopUpURL,
					"balance_usd_micros": 0,
					"message":            "Your organization's prepaid credits are depleted. Contact your org admin to add credits.",
				})
				return
			}
			// Infra error reading billing tables. Fail closed: a prepaid
			// gate that lets requests through on read errors creates an
			// unbilled-usage window. Return 503 so clients retry rather
			// than silently spending against an unknown balance.
			log.Error("Balance check failed; refusing request", "err", err, "organization_id", orgID)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":   "billing_unavailable",
				"message": "Billing system is temporarily unavailable. Retry in a few moments.",
			})
			return
		}

		if result.HasOverride {
			ctx := context.WithValue(c.Request.Context(), billing.HasOverrideContextKey, true)
			c.Request = c.Request.WithContext(ctx)
			c.Next()
			return
		}

		// Subscription-covered requests may run negative to a bounded overdraft
		// floor before gating (see SubscriptionOverdraftFloorMicros): the turn is
		// expected to be free but can fail over to a paid model, and we'd rather
		// stay optimistic than 402 free traffic at $0. Everyone else gates at
		// minBalanceMicros.
		threshold := minBalanceMicros
		if subscriptionExempt {
			threshold = billing.SubscriptionOverdraftFloorMicros
		}

		if result.BalanceMicros <= threshold {
			log.Info("Balance check rejected: balance at or below threshold",
				"organization_id", orgID,
				"balance_usd_micros", result.BalanceMicros,
				"threshold_usd_micros", threshold,
				"subscription_exempt", subscriptionExempt,
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
