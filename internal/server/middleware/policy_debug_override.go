package middleware

import (
	"context"
	"strconv"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// RouterPolicyDebugHeader overrides policy debug mode for authorized internal traffic.
const RouterPolicyDebugHeader = "x-weave-router-debug"

// WithPolicyDebugOverride applies persisted debug mode and an authorized
// per-request override. Invalid or unauthorized headers preserve the persisted value.
func WithPolicyDebugOverride() gin.HandlerFunc {
	return func(c *gin.Context) {
		installation := InstallationFrom(c)
		if installation == nil {
			c.Next()
			return
		}
		enabled := installation.PolicyDebugEnabled
		raw := strings.TrimSpace(c.GetHeader(RouterPolicyDebugHeader))
		if raw != "" {
			requested, err := strconv.ParseBool(raw)
			switch {
			case !installation.PolicyHeaderOverridesEnabled:
				observability.FromGin(c).Warn("Policy debug override ignored: installation is not authorized for policy headers", "installation_id", installation.ID)
			case err != nil:
				observability.FromGin(c).Warn("Policy debug override ignored: invalid boolean", "installation_id", installation.ID)
			default:
				enabled = requested
			}
		}
		ctx := c.Request.Context()
		ctx = contextWithPolicyDebug(ctx, enabled)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func contextWithPolicyDebug(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, proxy.PolicyDebugEnabledContextKey{}, enabled)
}
