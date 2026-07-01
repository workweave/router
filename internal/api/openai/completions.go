// Package openai holds HTTP handlers for the OpenAI Chat Completions surface.
package openai

import (
	"context"
	"errors"
	"io"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/bandit"
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/router/rl"
	"workweave/router/internal/server/middleware"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 10 * 1024 * 1024

func ChatCompletionHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
			return
		}
		if len(body) > maxBodyBytes {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large.")
			return
		}

		ctx := stashClientIdentity(c.Request.Context(), c.Request.Header)
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyOpenAIChatCompletion(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			var statusErr *providers.UpstreamStatusError
			if errors.As(err, &statusErr) {
				if c.Writer.Written() {
					return
				}
				writeOpenAIError(c, statusErr.Status, "api_error", "Upstream call failed.")
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if errors.Is(err, providers.ErrNotImplemented) {
				writeOpenAIError(c, http.StatusNotImplemented, "api_error", "Provider not implemented.")
				return
			}
			if errors.Is(err, proxy.ErrProviderNotConfigured) {
				writeOpenAIError(c, http.StatusBadGateway, "api_error", "Provider not configured.")
				return
			}
			if errors.Is(err, translate.ErrNotJSONObject) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
				return
			}
			if errors.Is(err, cluster.ErrNoEligibleProvider) {
				log.Warn("No eligible provider for request", "err", err)
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "No provider keys available for any deployed model: register a BYOK key or supply a provider Authorization header.")
				return
			}
			if errors.Is(err, cluster.ErrInvalidRoutingKnobs) {
				log.Warn("Invalid routing knobs supplied", "err", err)
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "Invalid routing knobs supplied.")
				return
			}
			if errors.Is(err, rl.ErrPolicyUnavailable) {
				log.Error("RL routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeOpenAIError(c, http.StatusServiceUnavailable, "api_error", "Router unavailable: RL policy router failed and no fallback is configured.")
				return
			}
			if errors.Is(err, bandit.ErrBanditUnavailable) {
				log.Error("Bandit routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeOpenAIError(c, http.StatusServiceUnavailable, "api_error", "Router unavailable: bandit router failed and no fallback is configured.")
				return
			}
			if errors.Is(err, cluster.ErrClusterUnavailable) {
				log.Error("Cluster routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeOpenAIError(c, http.StatusServiceUnavailable, "api_error", "Router unavailable: cluster scorer failed and no fallback is configured.")
				return
			}
			log.Error("Proxy failed", "err", err)
			writeOpenAIError(c, http.StatusBadGateway, "api_error", "Upstream call failed.")
			return
		}
	}
}

func stashClientIdentity(ctx context.Context, h http.Header) context.Context {
	id := proxy.ClientIdentity{
		SessionID:   proxy.NormalizeClientIdentifier(h.Get("X-Claude-Code-Session-Id")),
		Email:       proxy.NormalizeEmail(h.Get("X-Weave-User-Email")),
		DisplayName: proxy.NormalizeDisplayName(h.Get("X-Weave-User-Name")),
		UserAgent:   h.Get("User-Agent"),
		ClientApp:   proxy.NormalizeClientApp(h.Get("X-App"), h.Get("User-Agent")),
		RolloutID:   proxy.NormalizeRolloutID(h.Get(proxy.RolloutIDHeader)),
	}
	return context.WithValue(ctx, proxy.ClientIdentityContextKey{}, id)
}

func writeOpenAIError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}
