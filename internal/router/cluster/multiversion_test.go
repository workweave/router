package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

// scorerForVersion tags the canonical 2-cluster fixture with an
// arbitrary version label so override-picks-requested can be asserted.
func scorerForVersion(t *testing.T, version string, embedder Embedder) *Scorer {
	t.Helper()
	cb, rb, regb := twoClusterArtifacts(t)
	bundle := bundleFromBlobs(t, version, cb, rb, regb)
	s, err := NewScorer(bundle, cfgForTest(), embedder, allProviders())
	require.NoError(t, err)
	return s
}

func TestMultiversion_DefaultUsedWhenNoOverride(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v01 := scorerForVersion(t, "v0.1", emb)
	v02 := scorerForVersion(t, "v0.2", emb)

	multi, err := NewMultiversion("v0.2", map[string]*Scorer{"v0.1": v01, "v0.2": v02})
	require.NoError(t, err)

	got, err := multi.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
	})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.2", "no context override → default version answers")
}

func TestMultiversion_OverridePicksRequestedVersion(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v01 := scorerForVersion(t, "v0.1", emb)
	v02 := scorerForVersion(t, "v0.2", emb)

	multi, err := NewMultiversion("v0.2", map[string]*Scorer{"v0.1": v01, "v0.2": v02})
	require.NoError(t, err)

	ctx := WithVersion(context.Background(), "v0.1")
	got, err := multi.Route(ctx, router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.1", "context override → that version's scorer answers")
}

// Soft-degradation we keep: unknown override → default scorer (WARN
// logged). Eval harness typos shouldn't 503.
func TestMultiversion_UnknownVersionFallsBackToDefault(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v02 := scorerForVersion(t, "v0.2", emb)

	multi, err := NewMultiversion("v0.2", map[string]*Scorer{"v0.2": v02})
	require.NoError(t, err)

	ctx := WithVersion(context.Background(), "v0.99")
	got, err := multi.Route(ctx, router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.2", "unknown override version must fall back to default, not error")
}

func TestNewMultiversion_RejectsUnknownDefault(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v01 := scorerForVersion(t, "v0.1", emb)
	_, err := NewMultiversion("v0.99", map[string]*Scorer{"v0.1": v01})
	require.Error(t, err, "default version not in built versions must fail boot")
	assert.Contains(t, err.Error(), "v0.99")
}

func TestNewMultiversion_RejectsEmptyDefault(t *testing.T) {
	_, err := NewMultiversion("", map[string]*Scorer{})
	require.Error(t, err)
}

func TestVersionFromContext_EmptyByDefault(t *testing.T) {
	assert.Empty(t, VersionFromContext(context.Background()))
}

func TestWithVersion_EmptyStringIsNoOp(t *testing.T) {
	ctx := WithVersion(context.Background(), "")
	assert.Empty(t, VersionFromContext(ctx))
}

// DefaultDeployedModels must read the DEFAULT version's candidate list, not
// just any built version — the two fixture scorers below carry deliberately
// different registries so the assertion breaks if Multiversion picked the
// wrong one.
func TestMultiversion_DefaultDeployedModels_ReadsDefaultVersionOnly(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v01 := scorerForVersion(t, "v0.1", emb) // twoClusterArtifacts: opus + haiku

	cb, rb, regb := twoProviderArtifacts(t) // gpt-5 + opus
	v02 := bundleFromBlobs(t, "v0.2", cb, rb, regb)
	v02Scorer, err := NewScorer(v02, cfgForTest(), emb, allProviders())
	require.NoError(t, err)

	multi, err := NewMultiversion("v0.2", map[string]*Scorer{"v0.1": v01, "v0.2": v02Scorer})
	require.NoError(t, err)

	got := multi.DefaultDeployedModels()
	models := make([]string, 0, len(got))
	for _, e := range got {
		models = append(models, e.Model)
	}
	assert.ElementsMatch(t, []string{"claude-opus-4-7", "gpt-5"}, models,
		"DefaultDeployedModels must return the default version's (v0.2) candidates, not v0.1's opus+haiku pair")
}

// DefaultRoutingDistribution must delegate to the default version's Scorer
// with the caller's exact grid/exclusion arguments — verified by checking
// the projection matches calling RoutingDistribution directly on that Scorer.
func TestMultiversion_DefaultRoutingDistribution_DelegatesToDefaultScorer(t *testing.T) {
	bundle, err := LoadBundle("v0.67")
	require.NoError(t, err)
	require.True(t, bundle.IsV2, "test needs a v2 bundle; v1 bundles error out of RoutingDistribution")
	s, err := NewScorer(bundle, DefaultConfig(), &fakeEmbedder{dim: bundle.Centroids.Dim}, allProviders())
	require.NoError(t, err)

	multi, err := NewMultiversion("v0.67", map[string]*Scorer{"v0.67": s})
	require.NoError(t, err)

	const grid = 11
	want, err := s.RoutingDistribution(grid, nil, nil)
	require.NoError(t, err)

	got, err := multi.DefaultRoutingDistribution(grid, nil, nil)
	require.NoError(t, err)

	// Compare with a tolerance rather than assert.Equal: RoutingDistribution
	// tallies shares by iterating a map internally, so two independent calls
	// can accumulate the same floats in a different order and differ in the
	// last ULP without indicating any real behavioral divergence.
	require.Len(t, got, len(want))
	for i := range want {
		assert.Equal(t, want[i].QualityBias, got[i].QualityBias, "point %d dial position", i)
		assert.InDelta(t, want[i].ProjectedCostPer1KInputUSD, got[i].ProjectedCostPer1KInputUSD, 1e-9, "point %d projected cost", i)
		require.Len(t, got[i].Models, len(want[i].Models), "point %d model mix", i)
		for j := range want[i].Models {
			assert.Equal(t, want[i].Models[j].Model, got[i].Models[j].Model, "point %d model %d", i, j)
			assert.InDelta(t, want[i].Models[j].Share, got[i].Models[j].Share, 1e-9, "point %d model %d share", i, j)
		}
	}
}

// A default version whose Scorer disappears from the map after construction
// (shouldn't happen given NewMultiversion's invariant, but exercises the
// defensive branch) must surface ErrClusterUnavailable, not a generic error
// or a panic.
func TestMultiversion_DefaultRoutingDistribution_MissingDefaultReturnsClusterUnavailable(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	v01 := scorerForVersion(t, "v0.1", emb)
	multi, err := NewMultiversion("v0.1", map[string]*Scorer{"v0.1": v01})
	require.NoError(t, err)

	delete(multi.Versions, "v0.1")

	_, err = multi.DefaultRoutingDistribution(21, nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrClusterUnavailable)
}
