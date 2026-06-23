package admin

import (
	"net/http"
	"strconv"

	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

// RoutingDistributionSource projects the per-installation "quality vs price"
// dial's model mix across a grid of dial positions. Implemented by
// *cluster.Multiversion in production; callers can pass a fake in tests.
type RoutingDistributionSource interface {
	DefaultRoutingDistribution(gridN int) ([]cluster.DistributionPoint, error)
}

// maxDistributionGrid caps the requested grid size so a client can't ask the
// scorer to evaluate an unbounded number of dial positions.
const maxDistributionGrid = 101

type routingDistributionResponse struct {
	Points []cluster.DistributionPoint `json:"points"`
}

// RoutingDistributionHandler serves GET /v1/router/routing-distribution. For a
// grid of dial positions in [0, 1] it returns the projected model mix the
// QualityBias dial would produce (and the share-weighted input cost at each
// position). Read-only, unauthed metadata derived from the publicly published
// artifact — mounted like /v1/router/models so the Weave control plane can
// render the dashboard preview without juggling router API keys.
//
// `grid` (optional) sets the number of dial positions in [2, 101]; omitted
// leaves the scorer on its default grid.
func RoutingDistributionHandler(dist RoutingDistributionSource) gin.HandlerFunc {
	return func(c *gin.Context) {
		gridN := 0 // 0 -> scorer default
		if raw := c.Query("grid"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 2 || n > maxDistributionGrid {
				c.JSON(http.StatusBadRequest, gin.H{"error": "grid must be an integer in [2, 101]"})
				return
			}
			gridN = n
		}
		points, err := dist.DefaultRoutingDistribution(gridN)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "routing distribution unavailable"})
			return
		}
		c.JSON(http.StatusOK, routingDistributionResponse{Points: points})
	}
}

var _ RoutingDistributionSource = (*cluster.Multiversion)(nil)
