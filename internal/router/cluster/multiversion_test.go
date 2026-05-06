package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

// scorerForVersion builds a real Scorer using the canonical 2-cluster
// fixture but tagged with an arbitrary version label. Lets the
// multiversion test assert that per-request override actually picks the
// requested instance rather than always serving the default.
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

// TestMultiversion_UnknownVersionFallsBackToDefault: an unknown override
// version still serves traffic via the default scorer (logged at WARN).
// This is the soft-degradation we keep — eval harness typos shouldn't
// 503; the production-safe behavior is to serve the deployment default.
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
