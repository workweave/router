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

// DistributionPoint is the projected model mix at one dial position (shares
// sum to 1, modulo rounding); ProjectedCostPer1KInputUSD is the share-weighted
// input price for the dashboard's "spend vs all-Opus" readout.
type DistributionPoint struct {
	QualityBias                float64      `json:"quality_bias"`
	Models                     []ModelShare `json:"models"`
	ProjectedCostPer1KInputUSD float64      `json:"projected_cost_per_1k_input_usd"`
}

// defaultActiveKnobs returns a fresh copy of the bundle's default routing
// knobs (Alpha/AlphaFloor cloned so callers can mutate without leaking into
// shared defaults), falling back to alpha 0.53 on every cluster if the bundle
// ships none. Shared by Route and RoutingDistribution.
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

// allCentroidTopClusters computes each centroid's top-P nearest clusters,
// which depend only on cluster geometry, not on dial position or knobs.
// Shared by RoutingDistribution and computeDialCalibration so both compute it
// once instead of per grid step.
func (s *Scorer) allCentroidTopClusters() [][]int {
	k := s.centroids.K
	centroidTopClusters := make([][]int, k)
	for c := 0; c < k; c++ {
		centroidTopClusters[c] = topPNearest(s.centroids.Row(c), s.centroids, s.cfg.TopP)
	}
	return centroidTopClusters
}

// defaultDistributionGrid is the default dial-position count: 21 points = 0.05
// steps, fine enough for a smooth dashboard curve.
const defaultDistributionGrid = 21

// RoutingDistribution projects the model mix the QualityBias dial would
// produce across gridN evenly spaced dial positions in [0, 1].
//
// Each cluster centroid is routed once via the same scoring path as live
// traffic (dialToAlpha -> blendScoresV2 -> argmax) and tallied with equal
// weight, so the result is a mix "across request types," NOT traffic-weighted.
//
// excludedModels/excludedProviders apply the same eligibility filter as
// Route: an excluded model never appears, and its clusters fall through to
// the next-best eligible model instead of vanishing from the mix.
//
// gridN < 2 falls back to defaultDistributionGrid. Errors on v1 bundles
// (no quality_means to disperse over).
func (s *Scorer) RoutingDistribution(gridN int, excludedModels, excludedProviders map[string]struct{}) ([]DistributionPoint, error) {
	if !s.isV2 {
		return nil, fmt.Errorf("%w: routing distribution requires a v2 bundle", ErrClusterUnavailable)
	}
	if gridN < 2 {
		gridN = defaultDistributionGrid
	}

	k := s.centroids.K

	// Eligibility is dial-independent: resolve once and reuse per grid step.
	eligible := s.eligibleForDistribution(excludedModels, excludedProviders)
	if len(eligible) == 0 {
		return nil, fmt.Errorf("exclusions leave no eligible candidates: %w", ErrNoEligibleProvider)
	}

	centroidTopClusters := s.allCentroidTopClusters()

	points := make([]DistributionPoint, 0, gridN)
	for g := 0; g < gridN; g++ {
		t := float64(g) / float64(gridN-1)

		knobs := s.defaultActiveKnobs()
		s.applyDialAlpha(t, knobs.Alpha, knobs.AlphaFloor)

		counts := make(map[string]int, len(eligible))
		for c := 0; c < k; c++ {
			scores := s.blendScoresV2(centroidTopClusters[c], knobs, eligible, nil, nil)
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
