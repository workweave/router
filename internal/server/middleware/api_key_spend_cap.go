package middleware

import (
	"net/http"

	"workweave/router/internal/billing"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// WithAPIKeySpendCap enforces a per-key lifetime spend cap. Attached only in
// managed mode and only after WithAuth, so the key below is populated and its
// spent_usd_micros is metered by the debit hook.
//
// The cap and spend-to-date are read FRESH from Postgres each request (via the
// billing service), not from the cached auth key — a cached key's spend would
// be stale for the cache TTL, letting a hot key overrun its cap. This mirrors
// the per-request balance read in WithBalanceCheck.
//
// A key with no cap passes through. A read error fails closed with 503, like
// the balance gate: a prepaid cap that lets requests through on read errors is
// an unbilled-usage hole. Spend is only known after a response settles, so a
// key can still overshoot by at most one in-flight request's cost.
func WithAPIKeySpendCap(svc *billing.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)
		if _, ok := proxy.AgentShadowEvalFromContext(c.Request.Context()); ok {
			c.Next()
			return
		}

		apiKey := APIKeyFrom(c)
		if apiKey == nil || apiKey.ID == "" {
			// Admin-cookie sessions and other non-keyed paths carry no api key.
			c.Next()
			return
		}

		// The cap bounds PAID spend, not free subscription usage: a usage-bypass
		// org presenting a Claude/Codex credential that covers this route serves at
		// $0 on the caller's own plan, so exempt it from the cap-reached 402 below
		// (mirrors WithBalanceCheck). This gate keys off the api key, so read the
		// installation from context to check UsageBypassEnabled.
		installation := InstallationFrom(c)
		subscriptionExempt := installation != nil && installation.UsageBypassEnabled &&
			proxy.RequestPresentsCoveringSubscription(c.Request.Context(), c.Request.Header, c.FullPath())

		result, err := svc.CheckAPIKeySpendCap(c.Request.Context(), apiKey.ID)
		if err != nil {
			log.Error("API key spend-cap check failed; refusing request", "err", err, "api_key_id", apiKey.ID)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error":   "billing_unavailable",
				"message": "Billing system is temporarily unavailable. Retry in a few moments.",
			})
			return
		}

		if !result.Found || result.CapMicros == nil {
			c.Next()
			return
		}

		if result.SpentMicros >= *result.CapMicros {
			if subscriptionExempt {
				// Not 402'd: flag subscription-only so the proxy serves on the
				// caller's own subscription (or refuses a would-be-paid turn) and
				// never fails over to a paid model. Paid spend stays bounded at the cap.
				log.Info("API key spend cap reached but subscription covers the route: serving subscription-only",
					"api_key_id", apiKey.ID,
					"spent_usd_micros", result.SpentMicros,
					"spend_cap_usd_micros", *result.CapMicros,
				)
				c.Request = c.Request.WithContext(billing.WithSubscriptionOnly(c.Request.Context()))
				c.Next()
				return
			}
			log.Info("Request rejected: api key spend cap reached",
				"api_key_id", apiKey.ID,
				"spent_usd_micros", result.SpentMicros,
				"spend_cap_usd_micros", *result.CapMicros,
			)
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error":                "key_spend_cap_reached",
				"spent_usd_micros":     result.SpentMicros,
				"spend_cap_usd_micros": *result.CapMicros,
				"message":              "This router key has reached its spend cap. Mint a new key or raise the cap to continue.",
			})
			return
		}
		c.Next()
	}
}
