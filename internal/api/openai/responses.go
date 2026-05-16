package openai

import (
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

// ResponsesHandler accepts OpenAI Responses API requests (the surface that
// Codex CLI requires after wire_api="chat" was retired) and routes them
// through the chat-completions proxy via translate.ResponsesToChatCompletions.
// Errors are written in the Responses error shape, which matches the OpenAI
// chat error envelope verbatim, so writeOpenAIError is reused.
func ResponsesHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
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
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyOpenAIResponses(c.Request.Context(), body, c.Writer, c.Request); err != nil {
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
