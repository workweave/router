package middleware

import (
	"net/http"

	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

// WithAPIKeySpendCap enforces a per-key lifetime spend cap. Attached only in
// managed mode and only after WithAuth, so the key below is populated and its
// spent_usd_micros is metered by the debit hook.
//
// A key with no cap (SpendCapUsdMicros == nil) passes through untouched — the
// default for every key. Once a capped key's cumulative spend reaches its cap
// it is rejected with HTTP 402 and a distinct `key_spend_cap_reached` reason so
// callers can tell a per-key cap apart from a depleted org balance.
//
// The check reads the key already loaded by WithAuth (no extra query). Spend is
// only known after a response settles, so a key can overshoot its cap by at
// most one in-flight request's cost — the same prepaid semantics as the org
// balance gate.
func WithAPIKeySpendCap() gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := APIKeyFrom(c)
		if apiKey == nil || apiKey.SpendCapUsdMicros == nil {
			c.Next()
			return
		}
		if apiKey.SpentUsdMicros >= *apiKey.SpendCapUsdMicros {
			observability.FromGin(c).Info("Request rejected: api key spend cap reached",
				"api_key_id", apiKey.ID,
				"spent_usd_micros", apiKey.SpentUsdMicros,
				"spend_cap_usd_micros", *apiKey.SpendCapUsdMicros,
			)
			c.AbortWithStatusJSON(http.StatusPaymentRequired, gin.H{
				"error":                "key_spend_cap_reached",
				"spent_usd_micros":     apiKey.SpentUsdMicros,
				"spend_cap_usd_micros": *apiKey.SpendCapUsdMicros,
				"message":              "This router key has reached its spend cap. Mint a new key or raise the cap to continue.",
			})
			return
		}
		c.Next()
	}
}
