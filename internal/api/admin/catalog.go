package admin

import (
	"context"
	"net/http"
	"strings"

	"workweave/router/internal/observability"
	"workweave/router/internal/router"
	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

// CatalogModelsResponse is the shape returned by GET /v1/router/models.
// Kept stable so the Weave control plane can rely on the wire format without
// re-checking the artifact JSON shape on every router gitlink bump.
type CatalogModelsResponse struct {
	Models []deployedModelDTO `json:"models"`
}

// HMMRosterSource exposes the models the HMM policy strategy actually routes
// across (the sidecar roster arms mapped to catalog entries), which differs
// from the cluster artifact's registry that DeployedModelsSource reports.
type HMMRosterSource interface {
	HMMDeployedModels(ctx context.Context) ([]cluster.DeployedEntry, error)
}

// CatalogModelsHandler returns the deployed-models catalog for the caller's
// routing strategy. Read-only, unauthed metadata — mounted in both selfhosted
// and managed deployment modes so the Weave control plane (which is the only
// caller in managed mode) can fetch it without juggling router API keys.
//
// Without a ?strategy= query (or for the cluster strategy) it returns the
	// For ?strategy=hmm* returns the HMM sidecar roster; nil hmmModels falls back to the cluster list.
// The list is publicly known (we publish per-version model registries on the
// RouterArena leaderboard) so there is no leak risk from leaving this open.
func CatalogModelsHandler(models DeployedModelsSource, hmmModels HMMRosterSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		strategy := router.Strategy(strings.ToLower(strings.TrimSpace(c.Query("strategy"))))
		if router.IsHMMStrategy(strategy) && hmmModels != nil {
			entries, err := hmmModels.HMMDeployedModels(c.Request.Context())
			if err != nil {
				observability.FromGin(c).Error(
					"Failed to fetch HMM roster for deployed-models endpoint",
					"err", err,
					"strategy", string(strategy),
				)
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "hmm roster unavailable"})
				return
			}
			c.JSON(http.StatusOK, CatalogModelsResponse{Models: entriesToDTO(entries)})
			return
		}
		c.JSON(http.StatusOK, CatalogModelsResponse{Models: deployedModelsDTO(models)})
	}
}
