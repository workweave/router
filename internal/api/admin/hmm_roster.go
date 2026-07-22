package admin

import (
	"net/http"
	"sort"

	"workweave/router/internal/observability"
	"workweave/router/internal/router/hmm"
	"workweave/router/internal/router/policy"

	"github.com/gin-gonic/gin"
)

// hmmClusterDTO is one classifier cluster with its ordered default catalog
// model IDs (index 0 = highest serving priority).
type hmmClusterDTO struct {
	Cluster string   `json:"cluster"`
	Models  []string `json:"models"`
}

type hmmRosterResponse struct {
	Clusters     []hmmClusterDTO `json:"clusters"`
	RosterSHA256 string          `json:"roster_sha256"`
}

// HMMRosterHandler returns the frozen HMM roster with per-cluster arm lists
// mapped from roster IDs back to catalog model IDs, so the control plane can
// render the default arm order alongside the deployed-models universe. Unauthed
// like /v1/router/models: read-only and non-sensitive.
func HMMRosterHandler(source policy.RosterSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		snapshot, err := source.Roster(c.Request.Context())
		if err != nil {
			observability.FromGin(c).Warn("HMM roster fetch failed", "err", err)
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "hmm_roster_unavailable"})
			return
		}
		clusters := make([]hmmClusterDTO, 0, len(snapshot.Clusters))
		for cluster, arms := range snapshot.Clusters {
			models := make([]string, 0, len(arms))
			for _, arm := range arms {
				models = append(models, hmm.CatalogIDForRoster(arm))
			}
			clusters = append(clusters, hmmClusterDTO{Cluster: cluster, Models: models})
		}
		sort.SliceStable(clusters, func(i, j int) bool {
			return clusters[i].Cluster < clusters[j].Cluster
		})
		c.JSON(http.StatusOK, hmmRosterResponse{Clusters: clusters, RosterSHA256: snapshot.RosterSHA256})
	}
}
