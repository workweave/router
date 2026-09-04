package selection

import (
	"fmt"
	"sort"

	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/hmm"
	"weave-os/router/internal/router/hmm/rosterdata"
)

const defaultDistributionGrid = 21

// RoutingDistribution projects the HMM roster's within-band model mix across
// the quality/price dial. Each classifier band contributes equal weight; live
// traffic weights remain request-dependent and are intentionally not guessed.
func RoutingDistribution(roster *rosterdata.Roster, gridN int, excludedModels, excludedProviders map[string]struct{}) ([]cluster.DistributionPoint, error) {
	if roster == nil || (roster.SchemaVersion != rosterdata.SchemaVersionV7 && roster.SchemaVersion != rosterdata.SchemaVersionV75C) {
		return nil, fmt.Errorf("HMM routing distribution requires a dynamic v7 roster")
	}
	if gridN < 2 {
		gridN = defaultDistributionGrid
	}
	labels := make([]string, 0, len(roster.Clusters))
	for label := range roster.Clusters {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	points := make([]cluster.DistributionPoint, 0, gridN)
	for gridIndex := 0; gridIndex < gridN; gridIndex++ {
		qualityBias := float64(gridIndex) / float64(gridN-1)
		counts := make(map[string]int)
		prices := make(map[string]float64)
		selectedGroups := 0
		for _, label := range labels {
			clusterRoster := roster.Clusters[label]
			candidates := make(map[string]struct{}, len(clusterRoster.Arms))
			for _, arm := range clusterRoster.Arms {
				baseRosterID, _ := hmm.SplitEffort(arm)
				catalogID := hmm.CatalogIDForRoster(baseRosterID)
				model, ok := catalog.ByID(catalogID)
				if !ok {
					continue
				}
				provider := model.PrimaryProvider()
				if _, excluded := excludedModels[catalogID]; excluded {
					continue
				}
				if _, excluded := excludedProviders[provider]; excluded {
					continue
				}
				candidates[baseRosterID] = struct{}{}
				if len(model.Providers) > 0 {
					prices[catalogID] = model.Providers[0].Price.InputUSDPer1M / 1000
				}
			}
			pick, _, ok := SelectGroupsWithPreference(
				roster,
				[]Group{{Label: label}},
				"",
				candidates,
				&qualityBias,
			)
			if !ok {
				continue
			}
			selectedGroups++
			counts[hmm.CatalogIDForRoster(pick.Arm)]++
		}
		if len(counts) == 0 {
			return nil, fmt.Errorf("exclusions leave no eligible candidates: %w", cluster.ErrNoEligibleProvider)
		}
		modelShares := make([]cluster.ModelShare, 0, len(counts))
		projectedCost := 0.0
		for model, count := range counts {
			share := float64(count) / float64(selectedGroups)
			modelShares = append(modelShares, cluster.ModelShare{Model: model, Share: share})
			projectedCost += share * prices[model]
		}
		sort.Slice(modelShares, func(i, j int) bool {
			if modelShares[i].Share != modelShares[j].Share {
				return modelShares[i].Share > modelShares[j].Share
			}
			return modelShares[i].Model < modelShares[j].Model
		})
		points = append(points, cluster.DistributionPoint{
			QualityBias:                qualityBias,
			Models:                     modelShares,
			ProjectedCostPer1KInputUSD: projectedCost,
		})
	}
	return points, nil
}
