package openai

import (
	"io"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

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

		if err := svc.ProxyOpenAIResponses(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			cls, ok := proxy.ClassifyDispatchError(err)
			if ok && cls.Kind == proxy.DispatchErrorUpstreamStatus {
				if c.Writer.Written() {
					return
				}
				writeOpenAIError(c, cls.Status, "api_error", cls.Message)
				return
			}
			if c.Writer.Written() {
				log.Error("Proxy failed mid-stream", "err", err)
				return
			}
			if ok {
				proxy.LogDispatchErrorClass(log, cls, err)
				if cls.RetryAfter {
					c.Header("Retry-After", "1")
				}
				writeOpenAIError(c, cls.Status, openAIErrorType(cls.Kind), cls.Message)
				return
			}
			log.Error("Proxy failed", "err", err)
			writeOpenAIError(c, http.StatusBadGateway, "api_error", "Upstream call failed.")
			return
		}
	}
}
