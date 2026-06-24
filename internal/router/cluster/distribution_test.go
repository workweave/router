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
	require.NotEmpty(t, low.Models, "dial position 0.2 must be present in the 21-point grid")
	require.NotEmpty(t, mid.Models)
	assert.Greater(t, mid.ProjectedCostPer1KInputUSD, low.ProjectedCostPer1KInputUSD*1.5,
		"the 50%% dial must route a meaningfully pricier (higher-quality) mix than the 20%% dial")
}

// loadV0_70 loads the committed v0.70 bundle (the first shaped bundle whose
// default alpha protects agentic/code clusters at 0.96 and lets conversational
// clusters cheapen at 0.8). Like loadV0_67 it uses a fake embedder; the dial
// tests below never embed a prompt — they score cluster centroids directly.
func loadV0_70(t *testing.T) *Scorer {
	t.Helper()
	bundle, err := LoadBundle("v0.70")
	require.NoError(t, err)
	require.True(t, bundle.IsV2)
	s, err := NewScorer(bundle, DefaultConfig(), &fakeEmbedder{dim: bundle.Centroids.Dim}, allProviders())
	require.NoError(t, err)
	return s
}

// clusterWinnerAt scores the centroid of cluster c at the given dial position
// through the exact production path (defaultActiveKnobs -> applyDialAlpha ->
// blendScoresV2 -> argmax over the top-P union) and returns the winning model.
// withFloor=false reproduces the OLD uniform-dial behavior (the bug) so a test
// can assert the floor changed the realized routing.
func clusterWinnerAt(s *Scorer, c int, t float64, withFloor bool) string {
	knobs := s.defaultActiveKnobs()
	floor := knobs.AlphaFloor
	if !withFloor {
		floor = nil // reproduce the old uniform dial (no alpha_floor)
	}
	s.applyDialAlpha(t, knobs.Alpha, floor)
	top := topPNearest(s.centroids.Row(c), s.centroids, s.cfg.TopP)
	scores := s.blendScoresV2(top, knobs, s.models, nil)
	winner, _ := argmax(scores, s.models)
	return winner
}

func TestApplyDialAlpha_HoldsEachClusterAtItsDeclaredFloor(t *testing.T) {
	s := loadV0_70(t)
	knobs := s.defaultActiveKnobs()
	floor := knobs.AlphaFloor
	require.Len(t, floor, s.centroids.K, "v0.70 must ship a full per-cluster alpha_floor")

	// At the price extreme (t=0, dialToAlpha=0) every cluster is held at exactly
	// its declared floor — no cluster collapses to alpha 0 (the cheapest model).
	s.applyDialAlpha(0.0, knobs.Alpha, floor)
	for i := range knobs.Alpha {
		assert.InDelta(t, floor[i], knobs.Alpha[i], 1e-9,
			"cluster %d must be held at its declared floor at the price extreme", i)
	}

	// Above a cluster's floor the dial governs (max(dialAlpha, floor)); at the
	// quality extreme (t=1, dialToAlpha=1) every cluster reaches 1.0.
	knobs2 := s.defaultActiveKnobs()
	s.applyDialAlpha(1.0, knobs2.Alpha, knobs2.AlphaFloor)
	for i := range knobs2.Alpha {
		assert.InDelta(t, 1.0, knobs2.Alpha[i], 1e-9, "cluster %d must reach 1.0 at the quality extreme", i)
	}
}

func TestApplyDialAlpha_NilFloorIsUniformDial(t *testing.T) {
	// A bundle that ships no alpha_floor keeps the legacy uniform-dial behavior:
	// every cluster gets dialToAlpha(t) verbatim.
	s := loadV0_70(t)
	knobs := s.defaultActiveKnobs()
	s.applyDialAlpha(0.3, knobs.Alpha, nil)
	want := s.dialToAlpha(0.3)
	for i := range knobs.Alpha {
		assert.Equal(t, want, knobs.Alpha[i], "cluster %d must equal the uniform dial alpha with no floor", i)
	}
}

func TestApplyDialAlpha_AgenticStaysOffCheapModelAtLowDial(t *testing.T) {
	// The reported bug, end to end: under a price-leaning dial the agentic
	// cluster routed to minimax-m3 (a model that can't drive the Claude Code
	// skill/tool protocol). Cluster 0 is the agentic catch-all. Without the
	// floor (old uniform dial) the centroid routes to a cheap OSS model; with
	// the floor it stays on a frontier model.
	s := loadV0_70(t)
	const lowDial = 0.2

	without := clusterWinnerAt(s, 0, lowDial, false)
	with := clusterWinnerAt(s, 0, lowDial, true)

	assert.NotEqual(t, with, without,
		"the floor must change the agentic winner at a low dial (old=%s)", without)
	// The fixed winner must be a genuine agentic-capable frontier model, not the
	// cheap pack the uniform dial fell through to.
	frontier := map[string]struct{}{
		"claude-opus-4-8":          {},
		"claude-opus-4-7":          {},
		"claude-sonnet-4-6":        {},
		"gemini-3.1-pro-preview":   {},
		"gpt-5.5":                  {},
		"deepseek/deepseek-v4-pro": {},
	}
	_, isFrontier := frontier[with]
	assert.True(t, isFrontier, "with the floor the agentic cluster must route to a frontier model, got %s", with)
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
