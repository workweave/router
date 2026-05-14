package anthropic

import (
	"io"
	"net/http"

	"workweave/router/internal/observability"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/gin-gonic/gin"
)

func RouteHandler(svc *proxy.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		log := observability.FromGin(c)

		body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
		if err != nil {
			log.Debug("Failed to read request body", "err", err)
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
			return
		}

		env, parseErr := translate.ParseAnthropic(body)
		if parseErr != nil {
			writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
			return
		}

		ctx := c.Request.Context()
		embedFlag := svc.ResolveEmbedOnlyUserMessage(ctx)
		feats := env.RoutingFeatures(embedFlag)
		promptText := feats.PromptText
		if embedFlag && feats.OnlyUserMessageText != "" {
			promptText = feats.OnlyUserMessageText
		}
		decision, routeErr := svc.Route(ctx, router.Request{
			RequestedModel:       feats.Model,
			EstimatedInputTokens: feats.Tokens,
			HasTools:             feats.HasTools,
			PromptText:           promptText,
		})
		if routeErr != nil {
			log.Error("Routing failed", "err", routeErr)
			writeAnthropicError(c, http.StatusBadGateway, "api_error", "routing failed")
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"model":    decision.Model,
			"provider": decision.Provider,
			"reason":   decision.Reason,
		})
	}
}
