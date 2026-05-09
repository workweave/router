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

// RouterKeyHeader carries the Weave Router key when clients need to preserve
// Authorization / x-api-key for the upstream provider.
const RouterKeyHeader = "X-Weave-Router-Key"

// WithAuth validates the inbound request via a bearer rk_ token only. Used
// on data-plane routes (`/v1/*`) where the trust boundary is "valid router
// API key", not "logged-in admin browser." On failure, short-circuits 401.
//
// rk_ keys are installation-scoped and surface their
// installation/api-key/external-keys onto the request context the way they
// always have.
func WithAuth(svc *auth.Service) gin.HandlerFunc {
	return withAPIKey(svc)
}

// WithAdminOrAuth accepts either a signed admin session cookie OR a bearer
// rk_ token. Used on control-plane *read* routes (e.g. `/admin/v1/metrics/*`)
// so an installation can fetch its own metrics via its router API key,
// while the dashboard authenticates via the cookie set at
// `/admin/v1/auth/login`.
//
// **Do not use on `/v1/*` data-plane routes** — that would let a dashboard
// cookie call provider proxy endpoints, blurring control-plane and
// data-plane trust boundaries.
//
// **Do not use on control-plane *mutations*** (key issue/delete, provider
// key upsert/delete, config) — `rk_` holders should not be able to mint
// fresh API keys or rotate provider credentials for their installation;
// that path turns a leaked data-plane key into persistent control-plane
// access. Mount those endpoints under `WithAdminOnly` instead.
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

// WithAdminOnly requires a valid admin session cookie. Bearer rk_ tokens
// are explicitly rejected. Used on control-plane mutation endpoints
// (`/admin/v1/keys/*`, `/admin/v1/provider-keys/*`, `/admin/v1/config`) so
// a leaked installation API key can't be used to mint fresh credentials
// or rotate provider keys.
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

// withAPIKey is the bearer-only auth path shared by WithAuth and the
// fall-through branch of WithAdminOrAuth.
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

// tryAdminCookie verifies the admin session cookie if present. Returns nil
// (caller falls through to bearer auth) when there is no cookie, when admin
// login isn't configured, or when the cookie is invalid/expired.
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
		// Stale or tampered cookie: don't fail the request here — the user
		// might still be sending a valid rk_ bearer alongside it.
		return nil
	}
	return principal
}

// extractToken pulls the router token from the router-only header first, then
// falls back to legacy Authorization: Bearer or x-api-key credentials.
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
// nil for admin-cookie sessions (which are unscoped) and for requests that
// never went through WithAuth.
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

// AdminPrincipalFrom retrieves the admin principal stashed by WithAuth when
// the request authenticated via the session cookie. Returns nil for
// rk_-keyed requests and for requests that never went through WithAuth.
func AdminPrincipalFrom(c *gin.Context) *auth.AdminPrincipal {
	v, ok := c.Get(ctxKeyAdminPrincipal)
	if !ok {
		return nil
	}
	principal, _ := v.(*auth.AdminPrincipal)
	return principal
}
