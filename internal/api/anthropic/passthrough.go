package anthropic

import (
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

func PassthroughHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, proxy.MaxRequestBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read passthrough request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
			return
		}
		if len(body) > proxy.MaxRequestBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large.")
			return
		}

		if err := svc.PassthroughToProvider(c.Request.Context(), body, c.Writer, c.Request); err != nil {
			cls, ok := proxy.ClassifyDispatchError(err)
			if ok && cls.Kind == proxy.DispatchErrorUpstreamStatus {
				if c.Writer.Written() {
					return
				}
				writeAnthropicError(c, cls.Status, "api_error", cls.Message)
				return
			}
			if c.Writer.Written() {
				log.Error("Passthrough failed mid-stream", "err", err, "path", c.Request.URL.Path)
				return
			}
			if ok && cls.Kind == proxy.DispatchErrorNotImplemented {
				writeAnthropicError(c, cls.Status, anthropicErrorType(cls.Kind), cls.Message)
				return
			}
			log.Error("Passthrough failed", "err", err, "path", c.Request.URL.Path)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "Upstream call failed.")
			return
		}
	}
}
