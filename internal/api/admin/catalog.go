package admin

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CatalogModelsResponse is the shape returned by GET /v1/router/models.
// Kept stable so the Weave control plane can rely on the wire format without
// re-checking the artifact JSON shape on every router gitlink bump.
type CatalogModelsResponse struct {
	Models []deployedModelDTO `json:"models"`
}

// CatalogModelsHandler returns the deployed-models catalog for the active
// cluster artifact. Read-only, unauthed metadata — mounted in both selfhosted
// and managed deployment modes so the Weave control plane (which is the only
// caller in managed mode) can fetch it without juggling router API keys.
//
// The list is publicly known (we publish per-version model registries on the
// RouterArena leaderboard) so there is no leak risk from leaving this open.
func CatalogModelsHandler(models DeployedModelsSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, CatalogModelsResponse{Models: deployedModelsDTO(models)})
	}
}
