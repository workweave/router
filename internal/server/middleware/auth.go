package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

const (
	ctxKeyInstallation = "router_installation"
	ctxKeyAPIKey       = "router_api_key"
)

// WithAuth validates the inbound API key from either Authorization: Bearer
// or x-api-key headers. On failure, short-circuits with a generic 401.
func WithAuth(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		installation, apiKey, externalKeys, err := svc.VerifyAPIKey(c.Request.Context(), token)
		if err != nil {
			handleAuthError(c, err)
			return
		}
		c.Set(ctxKeyInstallation, installation)
		c.Set(ctxKeyAPIKey, apiKey)
		ctx := c.Request.Context()
		if apiKey != nil {
			ctx = context.WithValue(ctx, proxy.APIKeyIDContextKey{}, apiKey.ID)
		}
		if installation != nil && installation.ExternalID != "" {
			ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, installation.ExternalID)
		}
		if externalKeys != nil {
			ctx = context.WithValue(ctx, proxy.ExternalAPIKeysContextKey{}, externalKeys)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// extractToken pulls the bearer token from Authorization: Bearer or x-api-key.
func extractToken(c *gin.Context) string {
	if t := extractBearer(c.GetHeader("Authorization")); t != "" {
		return t
	}
	return strings.TrimSpace(c.GetHeader("x-api-key"))
}

func extractBearer(header string) string {
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(header) > len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

func handleAuthError(c *gin.Context, err error) {
	logger := observability.FromGin(c)
	switch {
	case errors.Is(err, auth.ErrInvalidPrefix):
		logger.Info("Auth rejected: invalid bearer prefix (expected rk_...)")
	case errors.Is(err, auth.ErrInvalidToken):
		logger.Info("Auth rejected: bearer token did not match an active key")
	default:
		// Genuine infrastructure failure (DB unreachable, etc.). Still 401 to
		// the caller; logged as Error for on-call.
		logger.Error("Auth check errored", "err", err)
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
}

// ExternalAPIKeysFrom retrieves the external API keys stashed by WithAuth.
// Returns nil when the request never went through WithAuth or when the
// installation has no BYOK keys configured.
func ExternalAPIKeysFrom(c *gin.Context) []*auth.ExternalAPIKey {
	v := c.Request.Context().Value(proxy.ExternalAPIKeysContextKey{})
	if v == nil {
		return nil
	}
	keys, _ := v.([]*auth.ExternalAPIKey)
	return keys
}

// InstallationFrom retrieves the authed installation set by WithAuth. Returns
// nil when the request never went through WithAuth.
func InstallationFrom(c *gin.Context) *auth.Installation {
	v, ok := c.Get(ctxKeyInstallation)
	if !ok {
		return nil
	}
	installation, _ := v.(*auth.Installation)
	return installation
}

func APIKeyFrom(c *gin.Context) *auth.APIKey {
	v, ok := c.Get(ctxKeyAPIKey)
	if !ok {
		return nil
	}
	apiKey, _ := v.(*auth.APIKey)
	return apiKey
}
