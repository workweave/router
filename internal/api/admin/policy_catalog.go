package admin

import (
	"net/http"
	"sort"

	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/policy"

	"github.com/gin-gonic/gin"
)

type policyCatalogEntry struct {
	Strategy     router.Strategy     `json:"strategy"`
	Available    bool                `json:"available"`
	Capabilities policy.Capabilities `json:"capabilities"`
}

// PolicyCatalogHandler exposes the strategy registry and optional capability
// surface to the control plane. It contains no tenant or request data.
func PolicyCatalogHandler(service *proxy.Service, defaultStrategy router.Strategy) gin.HandlerFunc {
	return func(c *gin.Context) {
		entries := []policyCatalogEntry{{
			Strategy:  router.StrategyCluster,
			Available: service != nil && service.PolicyStrategyAvailable(router.StrategyCluster),
			Capabilities: policy.Capabilities{
				SchemaVersion:          policy.SchemaVersionV1,
				HonorsPreferredModels:  true,
				HonorsQualityPriceBias: true,
				SupportsPreview:        true,
			},
		}}
		if service != nil {
			for _, strategy := range service.RegisteredStrategies() {
				capabilities, _ := service.PolicyCapabilities(strategy)
				entries = append(entries, policyCatalogEntry{
					Strategy:     strategy,
					Available:    service.PolicyStrategyAvailable(strategy),
					Capabilities: capabilities,
				})
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Strategy < entries[j].Strategy })
		c.JSON(http.StatusOK, gin.H{
			"schema_version":   policy.SchemaVersionV1,
			"default_strategy": defaultStrategy,
			"strategies":       entries,
		})
	}
}
