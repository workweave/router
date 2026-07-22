package middleware

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/gin-gonic/gin"
)

const (
	ctxKeyInstallation   = "router_installation"
	ctxKeyAPIKey         = "router_api_key"
	ctxKeyAdminPrincipal = "router_admin_principal"
)

// RouterKeyHeader carries the Weave Router key when clients need to preserve Authorization / x-api-key for the upstream provider.
const RouterKeyHeader = "X-Weave-Router-Key"

// AnthropicSubscriptionHeader carries a caller's Claude subscription OAuth
// token (sk-ant-oat-) alongside an rk_ router key, so the proxy can bill
// Claude-model turns to the caller's subscription instead of the deployment key.
const AnthropicSubscriptionHeader = "X-Weave-Anthropic-Subscription"

// OpenAISubscriptionHeader/OpenAIAccountIDHeader carry a caller's Codex
// (ChatGPT) OAuth JWT and its paired ChatGPT-Account-ID alongside an rk_
// router key, so Codex turns bill to the caller's ChatGPT plan. Both are
// required — the Codex backend 401/403s on a token without its account id.
const (
	OpenAISubscriptionHeader = "X-Weave-OpenAI-Subscription"
	OpenAIAccountIDHeader    = "X-Weave-OpenAI-Account-ID"
)

// WithAuth validates the inbound request via a bearer rk_ token only. Used on data-plane routes (`/v1/*`). On failure, short-circuits 401.
//
// byokDisabled strips BYOK (customer-owned provider) keys before downstream
// proxy code sees them. Managed-mode deployments pass true: they bill via
// prepaid credits, so honoring a leftover BYOK row would double-charge the
// customer. Self-hosted passes false; BYOK is the only credentialing path there.
func WithAuth(svc *auth.Service, byokDisabled bool) gin.HandlerFunc {
	return withAPIKey(svc, byokDisabled)
}

// WithAdminOrAuth accepts either a signed admin session cookie OR a bearer rk_ token.
// Don't use on `/v1/*` data-plane routes (a cookie shouldn't call provider proxy endpoints)
// or on control-plane mutations (a leaked rk_ shouldn't mint keys or rotate credentials —
// use WithAdminOnly there). See WithAuth for byokDisabled.
func WithAdminOrAuth(svc *auth.Service, byokDisabled bool) gin.HandlerFunc {
	apiKeyMW := withAPIKey(svc, byokDisabled)
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
//
// When byokDisabled is true, BYOK rows from svc.VerifyAPIKey are dropped
// before reaching the request context. Every downstream BYOK consumer
// (credential resolution, provider gating, usage bookkeeping) reads that
// single ctx key, so gating it here makes the whole path BYOK-blind.
func withAPIKey(svc *auth.Service, byokDisabled bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		installation, apiKey, externalKeys, clusterModelLists, err := svc.VerifyAPIKey(c.Request.Context(), token)
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
			if len(installation.ExcludedProviders) > 0 {
				ctx = context.WithValue(ctx, proxy.InstallationExcludedProvidersContextKey{}, installation.ExcludedProviders)
			}
			if len(installation.PreferredModels) > 0 {
				ctx = context.WithValue(ctx, proxy.InstallationPreferredModelsContextKey{}, installation.PreferredModels)
			}
			if installation.RoutingQualityWeight != nil {
				// User-facing dial position flows in as QualityBias (per-cluster,
				// dispersion-aware), not the uniform Alpha. See router.Overrides.
				ctx = context.WithValue(ctx, proxy.InstallationRoutingKnobsContextKey{}, &router.Overrides{
					QualityBias: installation.RoutingQualityWeight,
				})
			}
			if installation.UsageBypassEnabled {
				ctx = context.WithValue(ctx, proxy.InstallationUsageBypassContextKey{}, proxy.UsageBypassConfig{
					Enabled:   true,
					Threshold: installation.UsageBypassThreshold,
				})
			}
			if installation.SubscriptionRoutingDisabled {
				ctx = context.WithValue(ctx, proxy.InstallationSubscriptionRoutingDisabledContextKey{}, true)
			}
			if installation.RoutingRolloutID != "" {
				ctx = context.WithValue(ctx, proxy.PolicyRolloutIDContextKey{}, installation.RoutingRolloutID)
			}
			if installation.PolicyShadowStrategy != "" {
				ctx = context.WithValue(ctx, proxy.PolicyShadowStrategyContextKey{}, installation.PolicyShadowStrategy)
			}
			if installation.PolicyDebugEnabled {
				ctx = context.WithValue(ctx, proxy.PolicyDebugEnabledContextKey{}, true)
			}
			if installation.PolicyRoutingIntent != "" {
				ctx = context.WithValue(ctx, proxy.PolicyRoutingIntentContextKey{}, installation.PolicyRoutingIntent)
			}
			if installation.AITrainingAllowed {
				ctx = context.WithValue(ctx, proxy.PolicyTrainingAllowedContextKey{}, true)
			}
		}
		if externalKeys != nil && !byokDisabled {
			ctx = context.WithValue(ctx, proxy.ExternalAPIKeysContextKey{}, externalKeys)
		}
		if len(clusterModelLists) > 0 {
			overrides := make(map[string][]string, len(clusterModelLists))
			for _, list := range clusterModelLists {
				if len(list.Models) == 0 {
					continue
				}
				overrides[list.ClusterLabel] = list.Models
			}
			if len(overrides) > 0 {
				ctx = context.WithValue(ctx, proxy.ClusterModelListsContextKey{}, overrides)
			}
		}
		if installation != nil && installation.ID != "" {
			ctx = context.WithValue(ctx, proxy.InstallationIDContextKey{}, installation.ID)
		}
		// Stash the dedicated subscription header (router-keyed path) raw; the
		// proxy validates its shape and decides precedence. Never logged.
		if sub := strings.TrimSpace(c.GetHeader(AnthropicSubscriptionHeader)); sub != "" {
			ctx = context.WithValue(ctx, proxy.AnthropicSubscriptionContextKey{}, sub)
		}
		// Codex (ChatGPT) subscription: stash JWT + account ID raw for the proxy. Never logged.
		if sub := strings.TrimSpace(c.GetHeader(OpenAISubscriptionHeader)); sub != "" {
			ctx = context.WithValue(ctx, proxy.OpenAISubscriptionContextKey{}, sub)
		}
		if acct := strings.TrimSpace(c.GetHeader(OpenAIAccountIDHeader)); acct != "" {
			ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, acct)
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
		logger.Debug("Auth rejected: invalid bearer prefix (expected rk_...)")
	case errors.Is(err, auth.ErrInvalidToken):
		logger.Debug("Auth rejected: bearer token did not match an active key")
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
