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
		// Codex merges remote models into its file cache, so returning an empty
		// list is safe — the previous cached catalog stays intact.
		c.JSON(http.StatusOK, codexModelsResponse{Models: []struct{}{}})
	}
}
