package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"workweave/router/internal/router/cluster"

	"github.com/gin-gonic/gin"
)

// RoutingDistributionSource projects the per-installation "quality vs price"
// dial's model mix across a grid of dial positions, over the eligible pool left
// by excludedModels / excludedProviders. Implemented by *cluster.Multiversion
// in production; callers can pass a fake in tests.
type RoutingDistributionSource interface {
	DefaultRoutingDistribution(gridN int, excludedModels, excludedProviders map[string]struct{}) ([]cluster.DistributionPoint, error)
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
//
// `excluded_models` / `excluded_providers` (optional, comma-separated) narrow
// the eligible pool so the preview matches what Route would do for an
// installation with those exclusions — the control plane passes the requesting
// org's lists, keeping the endpoint unauthed/global while still org-correct.
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
		excludedModels := parseCSVSet(c.Query("excluded_models"))
		excludedProviders := parseCSVSet(c.Query("excluded_providers"))
		points, err := dist.DefaultRoutingDistribution(gridN, excludedModels, excludedProviders)
		if err != nil {
			// An exclusion set that empties the eligible pool is a client
			// configuration error (4xx), not a server outage — same sentinel
			// mapping cluster routing uses. Everything else is a 503.
			if errors.Is(err, cluster.ErrNoEligibleProvider) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "exclusions leave no eligible models"})
				return
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "routing distribution unavailable"})
			return
		}
		c.JSON(http.StatusOK, routingDistributionResponse{Points: points})
	}
}

// parseCSVSet splits a comma-separated query value into a set, trimming
// whitespace and dropping empties. Returns nil for an empty/blank input so the
// scorer keeps the full roster.
func parseCSVSet(raw string) map[string]struct{} {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out[v] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var _ RoutingDistributionSource = (*cluster.Multiversion)(nil)
