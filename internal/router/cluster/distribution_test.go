package cluster

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveClusterAlpha_EndpointsPinForEveryRank(t *testing.T) {
	for _, rank := range []float64{0, 0.25, 0.5, 0.75, 1} {
		assert.Equal(t, 0.0, deriveClusterAlpha(0, rank, qualityBiasDispersionSpread),
			"t=0 must map to alpha 0 (all price) regardless of dispersion rank %v", rank)
		assert.Equal(t, 1.0, deriveClusterAlpha(1, rank, qualityBiasDispersionSpread),
			"t=1 must map to alpha 1 (all quality) regardless of dispersion rank %v", rank)
	}
}

func TestDeriveClusterAlpha_MonotonicInDial(t *testing.T) {
	for _, rank := range []float64{0, 0.5, 1} {
		prev := -1.0
		for g := 0; g <= 20; g++ {
			a := deriveClusterAlpha(float64(g)/20, rank, qualityBiasDispersionSpread)
			assert.GreaterOrEqual(t, a, prev, "alpha must be non-decreasing in the dial (rank %v)", rank)
			prev = a
		}
	}
}

func TestDeriveClusterAlpha_HigherDispersionFavorsQualityEarlier(t *testing.T) {
	// At a fixed mid-dial position, a higher-dispersion cluster (one whose
	// strong models are much better than its cheap ones) should carry a higher
	// quality weight than a low-dispersion cluster — that's what spreads the
	// per-cluster crossovers into a gradient instead of a cliff.
	for _, dial := range []float64{0.25, 0.5, 0.75} {
		low := deriveClusterAlpha(dial, 0.0, qualityBiasDispersionSpread)
		mid := deriveClusterAlpha(dial, 0.5, qualityBiasDispersionSpread)
		high := deriveClusterAlpha(dial, 1.0, qualityBiasDispersionSpread)
		assert.Less(t, low, mid, "dial=%v: low-dispersion alpha should trail median", dial)
		assert.Less(t, mid, high, "dial=%v: median alpha should trail high-dispersion", dial)
	}
}

func TestComputeQualityDispersionRank_OrdersBySpread(t *testing.T) {
	models := []string{"a", "b"}
	// cluster 0: spread 0 (both equal); cluster 1: spread 1; cluster 2: spread 4.
	qm := Rankings{
		0: {"a": 1, "b": 1},
		1: {"a": 1, "b": 2},
		2: {"a": 1, "b": 5},
	}
	ranks := computeQualityDispersionRank(qm, models, 3)
	require.Len(t, ranks, 3)
	assert.Equal(t, 0.0, ranks[0], "lowest-dispersion cluster ranks 0")
	assert.Equal(t, 0.5, ranks[1], "middle-dispersion cluster ranks 0.5")
	assert.Equal(t, 1.0, ranks[2], "highest-dispersion cluster ranks 1")
}

func TestComputeQualityDispersionRank_SingleClusterNeutral(t *testing.T) {
	ranks := computeQualityDispersionRank(Rankings{0: {"a": 1, "b": 9}}, []string{"a", "b"}, 1)
	require.Len(t, ranks, 1)
	assert.Equal(t, 0.5, ranks[0], "with no other cluster to rank against, rank is neutral 0.5")
}

// loadV0_67 loads the committed v0.67 bundle through a fake embedder (matching
// its jina-v2 id / 768 dim). RoutingDistribution never embeds, so no ONNX
// runtime is needed; this keeps the test hermetic and in the default matrix.
func loadV0_67(t *testing.T) *Scorer {
	t.Helper()
	bundle, err := LoadBundle("v0.67")
	require.NoError(t, err)
	require.True(t, bundle.IsV2)
	s, err := NewScorer(bundle, DefaultConfig(), &fakeEmbedder{dim: bundle.Centroids.Dim}, allProviders())
	require.NoError(t, err)
	return s
}

func TestRoutingDistribution_EndpointsAndGradient(t *testing.T) {
	s := loadV0_67(t)
	const grid = 21
	points, err := s.RoutingDistribution(grid)
	require.NoError(t, err)
	require.Len(t, points, grid)

	// Shares sum to ~1 at every dial position.
	for _, p := range points {
		var sum float64
		for _, m := range p.Models {
			sum += m.Share
		}
		assert.InDelta(t, 1.0, sum, 1e-9, "shares must sum to 1 at quality_bias=%v", p.QualityBias)
	}

	// Price endpoint: pure cost → the single cheapest model wins every cluster.
	require.NotEmpty(t, points[0].Models)
	assert.Equal(t, 1.0, points[0].Models[0].Share, "at the price extreme one cheap model should take ~all traffic")

	// The endpoints are the cost extremes: cheapest mix at t=0, priciest at t=1.
	minCost, maxCost := points[0].ProjectedCostPer1KInputUSD, points[0].ProjectedCostPer1KInputUSD
	for _, p := range points {
		minCost = math.Min(minCost, p.ProjectedCostPer1KInputUSD)
		maxCost = math.Max(maxCost, p.ProjectedCostPer1KInputUSD)
	}
	assert.Equal(t, minCost, points[0].ProjectedCostPer1KInputUSD, "price end should be the cheapest point")
	assert.Equal(t, maxCost, points[len(points)-1].ProjectedCostPer1KInputUSD, "quality end should be the priciest point")
	assert.Greater(t, maxCost, minCost, "the dial must actually move projected cost")

	// Gradient, not cliff: several interior dial positions land strictly
	// between the two cost extremes (a cliff would jump from min to max with
	// nothing in between).
	span := maxCost - minCost
	interiorBetween := 0
	for _, p := range points[1 : len(points)-1] {
		if p.ProjectedCostPer1KInputUSD > minCost+0.02*span && p.ProjectedCostPer1KInputUSD < maxCost-0.02*span {
			interiorBetween++
		}
	}
	assert.GreaterOrEqual(t, interiorBetween, 3,
		"expected a gradient of intermediate mixes across the dial, got %d interior points between the extremes", interiorBetween)
}

func TestRoutingDistribution_DefaultGridAndV1Guard(t *testing.T) {
	s := loadV0_67(t)
	points, err := s.RoutingDistribution(0) // 0 -> default grid
	require.NoError(t, err)
	assert.Len(t, points, defaultDistributionGrid)
}
