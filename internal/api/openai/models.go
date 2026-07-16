package openai

import (
	"net/http"

	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

type codexModelsResponse struct {
	Models []struct{} `json:"models"`
}

// ModelsHandler returns the Codex `/models` shape ({models: [...]}) for Codex
// clients and delegates other clients to the existing provider passthrough.
func ModelsHandler(fallback gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := proxy.ClientIdentityFromHeaders(c.Request.Header)
		if id.ClientApp != proxy.ClientAppCodex {
			fallback(c)
			return
		}
		// Codex does NOT use this endpoint as the source-of-truth for model
		// discovery — it only refreshes from it periodically and merges the
		// result into its file cache. An empty list is a safe no-op: the
		// previous cached catalog stays intact.
		c.JSON(http.StatusOK, codexModelsResponse{})
	}
}
