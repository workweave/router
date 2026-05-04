package middleware

import (
	"context"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// EmbedLastUserMessageOverrideHeader flips the cluster scorer's PromptText
// source per-request. Only honored for allow-listed installations.
const EmbedLastUserMessageOverrideHeader = "x-weave-embed-last-user-message"

// WithEmbedLastUserMessageOverride attaches a bool override to the request
// context when the installation is allow-listed and the header is "true" or "false".
func WithEmbedLastUserMessageOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(EmbedLastUserMessageOverrideHeader))
		if raw == "" {
			c.Next()
			return
		}
		var override bool
		switch strings.ToLower(raw) {
		case "true":
			override = true
		case "false":
			override = false
		default:
			// Unrecognized values — ignore rather than 400. The harness
			// is the only legitimate caller and only sends "true"/"false";
			// any other value is misconfigured client noise we'd rather
			// not break the request on.
			c.Next()
			return
		}
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		if !installation.IsEvalAllowlisted {
			observability.FromGin(c).Debug(
				"Ignored embed-last-user-message override from non-allow-listed installation",
				"installation_id", installation.ID,
			)
			c.Next()
			return
		}
		ctx := context.WithValue(c.Request.Context(), proxy.EmbedLastUserMessageContextKey{}, override)
		c.Request = c.Request.WithContext(ctx)
		observability.FromGin(c).Info(
			"Embed-last-user-message override applied",
			"installation_id", installation.ID,
			"value", override,
		)
		c.Next()
	}
}
