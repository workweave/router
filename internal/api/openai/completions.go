// Package openai holds HTTP handlers for the OpenAI Chat Completions surface.
package openai

import (
	"errors"
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 10 * 1024 * 1024

// ChatCompletionHandler wires POST /v1/chat/completions to proxy.Service.ProxyOpenAIChatCompletion.
func ChatCompletionHandler(svc *proxy.Service) gin.HandlerFunc {
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
			log.Error("Proxy failed", "err", err)
			writeOpenAIError(c, http.StatusBadGateway, "api_error", "upstream call failed")
			return
		}
	}
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
