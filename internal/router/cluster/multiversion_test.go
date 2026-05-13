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

func TestParseAlphaSuffix(t *testing.T) {
	cases := []struct {
		in     string
		stem   string
		alpha  int
		wantOk bool
	}{
		{"v0.38-a00", "v0.38", 0, true},
		{"v0.38-a05", "v0.38", 5, true},
		{"v0.38-a10", "v0.38", 10, true},
		{"v0.38", "", 0, false},        // no suffix → not an alpha bundle
		{"v0.38-a11", "", 0, false},    // out of 0..10 range
		{"v0.38-a5", "", 0, false},     // not two-digit
		{"v0.38-aXY", "", 0, false},    // non-numeric
		{"plain", "", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			stem, alpha, ok := parseAlphaSuffix(tc.in)
			assert.Equal(t, tc.wantOk, ok)
			if tc.wantOk {
				assert.Equal(t, tc.stem, stem)
				assert.Equal(t, tc.alpha, alpha)
			}
		})
	}
}

func TestMultiversion_AlphaDispatchPicksMatchingBundle(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	a00 := scorerForVersion(t, "v0.38-a00", emb)
	a05 := scorerForVersion(t, "v0.38-a05", emb)
	a10 := scorerForVersion(t, "v0.38-a10", emb)

	multi, err := NewMultiversion("v0.38-a05", map[string]*Scorer{
		"v0.38-a00": a00,
		"v0.38-a05": a05,
		"v0.38-a10": a10,
	})
	require.NoError(t, err)

	tests := []struct {
		name        string
		alpha       int
		alphaSet    bool
		wantVersion string
	}{
		{"unset alpha serves default", 0, false, "v0.38-a05"},
		{"alpha=0 picks a00", 0, true, "v0.38-a00"},
		{"alpha=10 picks a10", 10, true, "v0.38-a10"},
		{"alpha=5 picks a05", 5, true, "v0.38-a05"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := multi.Route(context.Background(), router.Request{
				PromptText: strings.Repeat("x", 100),
				Alpha:      tc.alpha,
				AlphaSet:   tc.alphaSet,
			})
			require.NoError(t, err)
			assert.Contains(t, got.Reason, "cluster:"+tc.wantVersion)
		})
	}
}

// Installations whose alpha doesn't have a built bundle must serve the
// default scorer, not error. Missing-bundle is a deployment-time degradation
// (rolled out config before retraining) and shouldn't 503 every request.
func TestMultiversion_AlphaUnknownFallsBackToDefault(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	a05 := scorerForVersion(t, "v0.38-a05", emb)

	multi, err := NewMultiversion("v0.38-a05", map[string]*Scorer{"v0.38-a05": a05})
	require.NoError(t, err)

	got, err := multi.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
		Alpha:      8,
		AlphaSet:   true,
	})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.38-a05")
}

// Header override beats per-installation alpha so the eval harness can pin
// a specific bundle regardless of what an installation has saved.
func TestMultiversion_VersionHeaderBeatsAlpha(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	a00 := scorerForVersion(t, "v0.38-a00", emb)
	a05 := scorerForVersion(t, "v0.38-a05", emb)

	multi, err := NewMultiversion("v0.38-a05", map[string]*Scorer{
		"v0.38-a00": a00,
		"v0.38-a05": a05,
	})
	require.NoError(t, err)

	ctx := WithVersion(context.Background(), "v0.38-a00")
	got, err := multi.Route(ctx, router.Request{
		PromptText: strings.Repeat("x", 100),
		Alpha:      10, // would pick a10 if it were built, but header wins
		AlphaSet:   true,
	})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.38-a00")
}

// Legacy bundles (no -aNN suffix on the default) get no alpha-aware lookup;
// any alpha override is silently ignored and the default serves.
func TestMultiversion_LegacyDefaultIgnoresAlpha(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	legacy := scorerForVersion(t, "v0.37", emb)

	multi, err := NewMultiversion("v0.37", map[string]*Scorer{"v0.37": legacy})
	require.NoError(t, err)

	got, err := multi.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
		Alpha:      0,
		AlphaSet:   true,
	})
	require.NoError(t, err)
	assert.Contains(t, got.Reason, "cluster:v0.37")
}
