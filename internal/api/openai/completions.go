// Package openai holds HTTP handlers for the OpenAI Chat Completions surface.
package openai

import (
	"context"
	"io"
	"net/http"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/server/middleware"

	"github.com/gin-gonic/gin"
)

func ChatCompletionHandler(svc *proxy.Service, authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, proxy.MaxRequestBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
			return
		}
		if len(body) > proxy.MaxRequestBodyBytes {
			writeOpenAIError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large.")
			return
		}

		ctx := context.WithValue(c.Request.Context(), proxy.ClientIdentityContextKey{}, proxy.ClientIdentityFromHeaders(c.Request.Header))
		ctx = proxy.ResolveUserFromContext(ctx, authSvc, middleware.InstallationFrom(c))
		c.Request = c.Request.WithContext(ctx)

		if err := svc.ProxyOpenAIChatCompletion(c.Request.Context(), body, c.Writer, c.Request); err != nil {
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

// openAIErrorType maps a classified dispatch error to the OpenAI Chat
// Completions error envelope's "type" field. Like Anthropic, OpenAI only
// distinguishes client-input problems ("invalid_request_error") from
// everything else ("api_error").
func openAIErrorType(kind proxy.DispatchErrorKind) string {
	if kind.IsClientError() {
		return "invalid_request_error"
	}
	return "api_error"
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
