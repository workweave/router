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
	"workweave/router/internal/router/cluster"
	"workweave/router/internal/server/middleware"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 10 * 1024 * 1024

// ChatCompletionHandler wires POST /v1/chat/completions to proxy.Service.ProxyOpenAIChatCompletion.
// authSvc is used to upsert the end-user identity once email is read from
// X-Weave-User-Email; pass nil to skip user resolution (tests).
func ChatCompletionHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}

		ctx := stashClientIdentity(c.Request.Context(), c.Request.Header)
		ctx = resolveUser(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyOpenAIChatCompletion(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			var statusErr *providers.UpstreamStatusError
			if errors.As(err, &statusErr) {
				if c.Writer.Written() {
					return
				}
				writeOpenAIError(c, statusErr.Status, "api_error", "upstream call failed")
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if errors.Is(err, providers.ErrNotImplemented) {
				writeOpenAIError(c, http.StatusNotImplemented, "api_error", err.Error())
				return
			}
			if errors.Is(err, proxy.ErrProviderNotConfigured) {
				writeOpenAIError(c, http.StatusBadGateway, "api_error", err.Error())
				return
			}
			if errors.Is(err, translate.ErrNotJSONObject) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
				return
			}
			if errors.Is(err, cluster.ErrNoEligibleProvider) {
				log.Warn("No eligible provider for request", "err", err)
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "no provider keys available for any deployed model: register a BYOK key or supply a provider Authorization header")
				return
			}
			if errors.Is(err, cluster.ErrClusterUnavailable) {
				log.Error("Cluster routing unavailable", "err", err)
				c.Header("Retry-After", "1")
				writeOpenAIError(c, http.StatusServiceUnavailable, "api_error", "router unavailable: cluster scorer failed and no fallback is configured")
				return
			}
			log.Error("Proxy failed", "err", err)
			writeOpenAIError(c, http.StatusBadGateway, "api_error", "upstream call failed")
			return
		}
	}
}

// stashClientIdentity extracts user identification signals from HTTP headers
// and stashes them on the context. OpenAI-format requests don't carry the
// Anthropic metadata.user_id body field, so only headers are inspected.
func stashClientIdentity(ctx context.Context, h http.Header) context.Context {
	id := proxy.ClientIdentity{
		SessionID: h.Get("X-Claude-Code-Session-Id"),
		Email:     proxy.NormalizeEmail(h.Get("X-Weave-User-Email")),
		UserAgent: h.Get("User-Agent"),
		ClientApp: h.Get("X-App"),
	}
	return context.WithValue(ctx, proxy.ClientIdentityContextKey{}, id)
}

// resolveUser upserts the router user keyed on (installation, email) and
// returns a ctx with the user_id stashed. No-op when authSvc, installation,
// or the email signal is missing.
func resolveUser(ctx context.Context, authSvc *auth.Service, installation *auth.Installation) context.Context {
	if authSvc == nil || installation == nil {
		return ctx
	}
	id := proxy.ClientIdentityFrom(ctx)
	if id.Email == "" {
		return ctx
	}
	return authSvc.ResolveAndStashUser(ctx, installation.ID, id.Email, id.AccountID)
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
