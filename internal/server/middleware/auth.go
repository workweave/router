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
	ctxKeyInstallation   = "router_installation"
	ctxKeyAPIKey         = "router_api_key"
	ctxKeyAdminPrincipal = "router_admin_principal"
)

// RouterKeyHeader carries the Weave Router key when clients need to preserve Authorization / x-api-key for the upstream provider.
const RouterKeyHeader = "X-Weave-Router-Key"

// WithAuth validates the inbound request via a bearer rk_ token only. Used on data-plane routes (`/v1/*`). On failure, short-circuits 401.
func WithAuth(svc *auth.Service) gin.HandlerFunc {
	return withAPIKey(svc)
}

// WithAdminOrAuth accepts either a signed admin session cookie OR a bearer rk_ token.
//
// Do not use on `/v1/*` data-plane routes — a dashboard cookie must not call provider proxy endpoints.
// Do not use on control-plane mutations — a leaked rk_ must not mint fresh keys or rotate provider credentials; use WithAdminOnly instead.
func WithAdminOrAuth(svc *auth.Service) gin.HandlerFunc {
	apiKeyMW := withAPIKey(svc)
	return func(c *gin.Context) {
		if principal := tryAdminCookie(c, svc); principal != nil {
			c.Set(ctxKeyAdminPrincipal, principal)
			c.Next()
			return
		}
		apiKeyMW(c)
	}
}

// WithAdminOnly requires a valid admin session cookie; bearer rk_ tokens are rejected so a leaked installation API key can't mint credentials or rotate provider keys.
func WithAdminOnly(svc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !svc.AdminLoginEnabled() {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "admin_login_disabled"})
			return
		}
		principal := tryAdminCookie(c, svc)
		if principal == nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "admin_session_required"})
			return
		}
		c.Set(ctxKeyAdminPrincipal, principal)
		c.Next()
	}
}

// withAPIKey is the bearer-only auth path shared by WithAuth and the fall-through branch of WithAdminOrAuth.
func withAPIKey(svc *auth.Service) gin.HandlerFunc {
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
		if installation != nil {
			if installation.ExternalID != "" {
				ctx = context.WithValue(ctx, proxy.ExternalIDContextKey{}, installation.ExternalID)
			}
			if installation.ID != "" {
				ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, installation.ID)
			}
			if len(installation.ExcludedModels) > 0 {
				ctx = context.WithValue(ctx, proxy.InstallationExcludedModelsContextKey{}, installation.ExcludedModels)
			}
		}
		if externalKeys != nil {
			ctx = context.WithValue(ctx, proxy.ExternalAPIKeysContextKey{}, externalKeys)
		}
		if installation != nil && installation.ID != "" {
			ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, installation.ID)
		}
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// tryAdminCookie returns nil so callers fall through to bearer auth when the cookie is absent, admin login is disabled, or the cookie is invalid.
func tryAdminCookie(c *gin.Context, svc *auth.Service) *auth.AdminPrincipal {
	if !svc.AdminLoginEnabled() {
		return nil
	}
	cookie, err := c.Cookie(auth.AdminSessionCookieName)
	if err != nil || cookie == "" {
		return nil
	}
	principal, err := svc.VerifyAdminSession(cookie)
	if err != nil {
		// Stale/tampered cookie: don't fail — caller may still have a valid rk_ bearer.
		return nil
	}
	return principal
}

// extractToken pulls the router token from RouterKeyHeader first, then falls back to Authorization: Bearer or x-api-key.
func extractToken(c *gin.Context) string {
	if t := strings.TrimSpace(c.GetHeader(RouterKeyHeader)); t != "" {
		return t
	}
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
		// Infra failure (DB unreachable, etc.). Still 401 to the caller; logged as Error for on-call.
		logger.Error("Auth check errored", "err", err)
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_key"})
}

// InstallationFrom retrieves the authed installation set by WithAuth. Returns nil for admin-cookie sessions and unauthed requests.
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

// AdminPrincipalFrom retrieves the admin principal set when the request authenticated via the session cookie. Returns nil for rk_-keyed or unauthed requests.
func AdminPrincipalFrom(c *gin.Context) *auth.AdminPrincipal {
	v, ok := c.Get(ctxKeyAdminPrincipal)
	if !ok {
		return nil
	}
	principal, _ := v.(*auth.AdminPrincipal)
	return principal
}
