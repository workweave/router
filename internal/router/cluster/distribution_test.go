package cluster

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDialToAlpha_EndpointsPin(t *testing.T) {
	// The slider's extremes stay honest regardless of calibration: 0 -> all
	// cheapest, 1 -> best-per-cluster top quality.
	for _, bp := range [][]float64{nil, {0, 1}, {0, 0.3, 0.55, 0.87, 1}} {
		s := &Scorer{dialAlphaBreakpoints: bp}
		assert.Equal(t, 0.0, s.dialToAlpha(0), "t=0 must map to alpha 0 (breakpoints %v)", bp)
		assert.Equal(t, 1.0, s.dialToAlpha(1), "t=1 must map to alpha 1 (breakpoints %v)", bp)
		assert.Equal(t, 0.0, s.dialToAlpha(-0.5), "t<0 clamps to 0")
		assert.Equal(t, 1.0, s.dialToAlpha(1.5), "t>1 clamps to 1")
	}
}

func TestDialToAlpha_MonotonicAndInterpolates(t *testing.T) {
	// Breakpoints placed at equal dial spacing: with 5 of them the dial quarters
	// land exactly on a breakpoint, and interior positions interpolate linearly.
	s := &Scorer{dialAlphaBreakpoints: []float64{0, 0.3, 0.55, 0.87, 1}}
	assert.InDelta(t, 0.3, s.dialToAlpha(0.25), 1e-9, "quarter dial lands on the 2nd breakpoint")
	assert.InDelta(t, 0.55, s.dialToAlpha(0.5), 1e-9, "mid dial lands on the 3rd breakpoint")
	assert.InDelta(t, 0.425, s.dialToAlpha(0.375), 1e-9, "between breakpoints 2 and 3 it interpolates")

	prev := -1.0
	for g := 0; g <= 100; g++ {
		a := s.dialToAlpha(float64(g) / 100)
		assert.GreaterOrEqual(t, a, prev, "alpha must be non-decreasing in the dial")
		prev = a
	}
}

func TestDialToAlpha_NoCalibrationIsIdentity(t *testing.T) {
	// A bundle with no mix separation (or a v1 bundle) leaves breakpoints nil;
	// the dial then maps straight through so behavior is unchanged.
	s := &Scorer{dialAlphaBreakpoints: nil}
	for _, tt := range []float64{0.1, 0.4, 0.7, 0.9} {
		assert.InDelta(t, tt, s.dialToAlpha(tt), 1e-9, "identity fallback at t=%v", tt)
	}
}

func TestComputeDialCalibration_AscendingPinnedEndpoints(t *testing.T) {
	// On the real bundle the calibration must be a strictly ascending sequence
	// pinned at 0 and 1, with several interior breakpoints (the bundle has many
	// distinct routed mixes between the cheapest and the saturated end).
	s := loadV0_67(t)
	bp := s.dialAlphaBreakpoints
	require.GreaterOrEqual(t, len(bp), 4, "expected several mix breakpoints on the real bundle")
	assert.Equal(t, 0.0, bp[0], "first breakpoint pins the price extreme")
	assert.Equal(t, 1.0, bp[len(bp)-1], "last breakpoint pins the quality extreme")
	for i := 1; i < len(bp); i++ {
		assert.Greater(t, bp[i], bp[i-1], "breakpoints must be strictly ascending")
	}
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

func TestRoutingDistribution_NoDeadZone(t *testing.T) {
	// Regression guard for the reported bug: every adjacent pair of dial
	// positions should differ in either the routed mix or its projected cost.
	// A dead zone (a run of identical mixes) is exactly what made "50% look like
	// 20%"; the calibration must keep all but a small number of steps live.
	s := loadV0_67(t)
	points, err := s.RoutingDistribution(21)
	require.NoError(t, err)

	identicalRuns := 0
	for i := 1; i < len(points); i++ {
		samMix := mixSignatureOf(points[i].Models) == mixSignatureOf(points[i-1].Models)
		if samMix {
			identicalRuns++
		}
	}
	// A few coincidental repeats are fine (21 dial samples vs a finite set of
	// distinct mixes); a dead zone would repeat across many steps in a row.
	assert.LessOrEqual(t, identicalRuns, 3,
		"too many adjacent dial positions route an identical mix (%d) — dial has a dead zone", identicalRuns)
}

func TestRoutingDistribution_MidDialIsPricierThanLowDial(t *testing.T) {
	// The reported symptom in user terms: a mid dial (0.5) used to route the
	// same all-cheapest mix as a low dial (0.2). After calibration the mid dial
	// must route a meaningfully pricier (higher-quality) mix.
	s := loadV0_67(t)
	points, err := s.RoutingDistribution(21)
	require.NoError(t, err)

	var low, mid DistributionPoint
	for _, p := range points {
		if math.Abs(p.QualityBias-0.2) < 1e-9 {
			low = p
		}
		if math.Abs(p.QualityBias-0.5) < 1e-9 {
			mid = p
		}
	}
	require.NotEmpty(t, mid.Models)
	assert.Greater(t, mid.ProjectedCostPer1KInputUSD, low.ProjectedCostPer1KInputUSD*1.5,
		"the 50%% dial must route a meaningfully pricier (higher-quality) mix than the 20%% dial")
}

// mixSignatureOf renders a DistributionPoint's model shares as a stable key for
// comparing whether two dial positions route the same mix.
func mixSignatureOf(models []ModelShare) string {
	sorted := append([]ModelShare(nil), models...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Model < sorted[j].Model })
	var b strings.Builder
	for _, m := range sorted {
		fmt.Fprintf(&b, "%s:%.4f,", m.Model, m.Share)
	}
	return b.String()
}
