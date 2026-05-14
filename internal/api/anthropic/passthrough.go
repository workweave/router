package anthropic

import (
	"errors"
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

func PassthroughHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read passthrough request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}

		if err := svc.PassthroughToProvider(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			var statusErr *providers.UpstreamStatusError
			if errors.As(err, &statusErr) {
				if c.Writer.Written() {
					return
				}
				writeAnthropicError(c, statusErr.Status, "api_error", "upstream call failed")
				return
			}
			if c.Writer.Written() {
				log.Error("Passthrough failed mid-stream", "err", err, "path", c.Request.URL.Path)
				return
			}
			if errors.Is(err, providers.ErrNotImplemented) {
				writeAnthropicError(c, http.StatusNotImplemented, "api_error", "provider not configured")
				return
			}
			log.Error("Passthrough failed", "err", err, "path", c.Request.URL.Path)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "upstream call failed")
			return
		}
	}
}
