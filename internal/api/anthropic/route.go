package anthropic

import (
	"errors"
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

func RouteHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, proxy.MaxRequestBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to read request body.")
			return
		}
		if len(body) > proxy.MaxRequestBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large.")
			return
		}

		ctx := c.Request.Context()
		decision, routeErr := svc.RouteAnthropicRequest(ctx, body)
		if routeErr != nil {
			if errors.Is(routeErr, proxy.ErrRequestNotJSONObject) {
				writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Request body must be a JSON object.")
				return
			}
			if errors.Is(routeErr, cluster.ErrInvalidRoutingKnobs) {
				log.Warn("Invalid routing knobs supplied on route", "err", routeErr)
				writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Invalid routing knobs supplied.")
				return
			}
			log.Error("Routing failed", "err", routeErr)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "Routing failed.")
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"model":    decision.Model,
			"provider": decision.Provider,
			"reason":   decision.Reason,
		})
	}
}
