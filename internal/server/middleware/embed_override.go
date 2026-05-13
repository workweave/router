package middleware

import (
	"context"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// EmbedOnlyUserMessageOverrideHeader flips the cluster scorer's PromptText source per-request.
const EmbedOnlyUserMessageOverrideHeader = "x-weave-embed-only-user-message"

// WithEmbedOnlyUserMessageOverride attaches a bool override to the request context when the header is "true" or "false".
func WithEmbedOnlyUserMessageOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := strings.TrimSpace(c.GetHeader(EmbedOnlyUserMessageOverrideHeader))
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
			// Unrecognized values — ignore rather than 400 the request on misconfigured client noise.
			c.Next()
			return
		}
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		ctx := context.WithValue(c.Request.Context(), proxy.EmbedOnlyUserMessageContextKey{}, override)
		c.Request = c.Request.WithContext(ctx)
		observability.FromGin(c).Info(
			"Embed-only-user-message override applied",
			"installation_id", installation.ID,
			"value", override,
		)
		c.Next()
	}
}
