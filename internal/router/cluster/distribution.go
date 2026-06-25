package cluster

import (
	"fmt"
	"sort"
)

// ModelShare is one model's share of a projected routing distribution.
type ModelShare struct {
	Model string  `json:"model"`
	Share float64 `json:"share"` // fraction in [0, 1]
}

// DistributionPoint is the projected model mix at one dial position. Share
// values sum to 1 (modulo rounding). ProjectedCostPer1KInputUSD is the
// share-weighted input price, the basis for the dashboard's "spend vs all-Opus"
// readout.
type DistributionPoint struct {
	QualityBias                float64      `json:"quality_bias"`
	Models                     []ModelShare `json:"models"`
	ProjectedCostPer1KInputUSD float64      `json:"projected_cost_per_1k_input_usd"`
}

// defaultActiveKnobs returns a fresh copy of the bundle's default routing knobs
// (Alpha slice cloned so callers can mutate per request without leaking into
// the shared bundle defaults). When the bundle ships no defaults it falls back
// to a neutral alpha of 0.53 on every cluster. Single source of truth for both
// Route and RoutingDistribution.
func (s *Scorer) defaultActiveKnobs() DefaultRoutingKnobs {
	if s.metadata != nil && s.metadata.Training.DefaultRoutingKnobs != nil {
		knobs := *s.metadata.Training.DefaultRoutingKnobs
		knobs.Alpha = append([]float64(nil), knobs.Alpha...)
		knobs.AlphaFloor = append([]float64(nil), knobs.AlphaFloor...)
		return knobs
	}
	knobs := DefaultRoutingKnobs{
		Alpha:                make([]float64, s.centroids.K),
		SpeedWeight:          0.0,
		OutputCostRatio:      0.0,
		ExpectedOutputTokens: 2000,
		PerModelVerbosity:    false,
	}
	for i := range knobs.Alpha {
		knobs.Alpha[i] = 0.53
	}
	return knobs
}

// defaultDistributionGrid is the dial-position count used when a caller does
// not request a specific grid size: 21 points = steps of 0.05, fine enough for
// the dashboard to render a smooth curve and seat the slider handle.
const defaultDistributionGrid = 21

// RoutingDistribution projects the model mix the QualityBias dial would produce
// across a grid of gridN evenly spaced dial positions in [0, 1].
//
// Each cluster centroid is treated as one representative request, routed with
// the SAME scoring path as live traffic (dialToAlpha -> blendScoresV2 ->
// argmax), and the winners are tallied with equal weight. This makes the
// preview a faithful read of the routing math; what it is NOT is traffic
// weighted — every cluster counts once regardless of how much real traffic
// lands there, so the dashboard should frame it as the mix "across request
// types," not "your traffic."
//
// excludedModels / excludedProviders mirror the caller's per-installation
// exclusion lists: the preview routes over the SAME eligible pool Route would
// (a model is dropped when it is named in excludedModels or when every one of
// its provider bindings is excluded), so an excluded model never appears in the
// dial preview and the cluster it would have won falls through to the next-best
// eligible model rather than vanishing from the mix. Nil/empty sets leave the
// full deployed roster eligible.
//
// gridN < 2 falls back to defaultDistributionGrid. Returns an error for v1
// bundles, which have no quality_means to disperse over.
func (s *Scorer) RoutingDistribution(gridN int, excludedModels, excludedProviders map[string]struct{}) ([]DistributionPoint, error) {
	if !s.isV2 {
		return nil, fmt.Errorf("%w: routing distribution requires a v2 bundle", ErrClusterUnavailable)
	}
	if gridN < 2 {
		gridN = defaultDistributionGrid
	}

	k := s.centroids.K

	// Eligibility is dial-independent, so resolve the exclusion-filtered pool
	// once and reuse it across every grid step. An exclusion set that empties
	// the pool is rejected the same way Route rejects it, rather than returning
	// points whose shares sum to 0.
	eligible := s.eligibleForDistribution(excludedModels, excludedProviders)
	if len(eligible) == 0 {
		return nil, fmt.Errorf("exclusions leave no eligible candidates: %w", ErrNoEligibleProvider)
	}

	// Each centroid's top-P clusters depend only on cluster geometry, not on
	// the dial position, so compute them once instead of per grid step.
	centroidTopClusters := make([][]int, k)
	for c := 0; c < k; c++ {
		centroidTopClusters[c] = topPNearest(s.centroids.Row(c), s.centroids, s.cfg.TopP)
	}

	points := make([]DistributionPoint, 0, gridN)
	for g := 0; g < gridN; g++ {
		t := float64(g) / float64(gridN-1)

		knobs := s.defaultActiveKnobs()
		s.applyDialAlpha(t, knobs.Alpha, knobs.AlphaFloor)

		counts := make(map[string]int, len(eligible))
		for c := 0; c < k; c++ {
			scores := s.blendScoresV2(centroidTopClusters[c], knobs, eligible, nil)
			winner, _ := argmax(scores, eligible)
			if winner != "" {
				counts[winner]++
			}
		}

		shares := make([]ModelShare, 0, len(counts))
		var projCost float64
		for m, c := range counts {
			share := float64(c) / float64(k)
			shares = append(shares, ModelShare{Model: m, Share: share})
			if axis, ok := s.modelAxes[m]; ok && axis.InputPer1KUSD != nil {
				projCost += share * *axis.InputPer1KUSD
			}
		}
		sort.Slice(shares, func(a, b int) bool {
			if shares[a].Share != shares[b].Share {
				return shares[a].Share > shares[b].Share
			}
			return shares[a].Model < shares[b].Model
		})

		points = append(points, DistributionPoint{
			QualityBias:                t,
			Models:                     shares,
			ProjectedCostPer1KInputUSD: projCost,
		})
	}
	return points, nil
}
