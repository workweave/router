package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCandidateK12Loads is the load-gate for the candidate-k12 bake-off bundle:
// the prod-turn re-cluster (K=12) + in-house harness-faithful quality candidate
// must parse end-to-end, construct a Scorer, and route the offline-screen mix.
// It asserts the properties the staging bake-off relies on (K=12, 21 models,
// quality present for every cluster, fable-5/glm-5.2/sonnet-5 routable) and pins
// the per-cluster argmax to the offline screen's picks (glm-5.2/fable-5-led).
func TestCandidateK12Loads(t *testing.T) {
	bundle, err := LoadBundle("candidate-k12")
	require.NoError(t, err, "candidate-k12 must parse end-to-end")
	require.True(t, bundle.IsV2, "candidate-k12 is a v2 bundle (quality_means present)")

	// K=12 geometry.
	require.NotNil(t, bundle.Centroids)
	assert.Equal(t, 12, bundle.Centroids.K, "candidate-k12 must be a K=12 re-cluster")
	assert.Equal(t, 768, bundle.Centroids.Dim, "jina-v2-base-code-int8 is 768-dim")
	assert.Equal(t, EmbedderJinaV2, bundle.EmbedderID())
	assert.Equal(t, 768, bundle.EmbedDim())

	// 21 deployed models.
	models := bundle.Registry.Models()
	assert.Len(t, models, 21, "candidate-k12 roster is 21 models")

	// Quality present for every (cluster, model): the loader validates this, but
	// assert it explicitly so a regression is legible.
	for k := 0; k < bundle.Centroids.K; k++ {
		row, ok := bundle.QualityMeans[k]
		require.Truef(t, ok, "quality_means missing cluster %d", k)
		for _, m := range models {
			_, ok := row[m]
			require.Truef(t, ok, "quality_means cluster %d missing model %q", k, m)
		}
	}

	// The three headline models for this candidate must be in the roster + axes.
	for _, m := range []string{"claude-fable-5", "z-ai/glm-5.2", "claude-sonnet-5"} {
		assert.Contains(t, models, m, "%s must be a deployed model", m)
		_, ok := bundle.ModelAxes[m]
		assert.Truef(t, ok, "%s must have operational axes", m)
	}

	// Provider set covering every provider the 21-model roster binds to (the
	// staging bake-off deploy carries keys for all seven). Restricting to a
	// subset would drop OSS models (e.g. glm-5.2 on fireworks) from the
	// eligible pool and the per-cluster argmax would no longer reflect the
	// offline screen.
	providers := map[string]struct{}{
		"anthropic": {}, "openai": {}, "google": {},
		"fireworks": {}, "makora": {}, "openrouter": {}, "bedrock": {},
	}

	// Build a real Scorer through the jina-v2 fake embedder (matches id/dim, so
	// NewScorer's embedder guard passes without ONNX).
	s, err := NewScorer(bundle, DefaultConfig(), &fakeEmbedder{dim: bundle.Centroids.Dim}, providers)
	require.NoError(t, err, "candidate-k12 must construct a Scorer")

	// fable-5, glm-5.2, sonnet-5 must be routable (resolvable provider binding).
	routable := RoutableModelSet(bundle.Registry, providers)
	for _, m := range []string{"claude-fable-5", "z-ai/glm-5.2", "claude-sonnet-5"} {
		_, ok := routable[m]
		assert.Truef(t, ok, "%s must be routable", m)
	}

	// Per-cluster argmax through the live v2 blend (the authoritative runtime
	// path: blendScoresV2 over a single cluster with the bundle's default knobs =
	// alpha 0.7). Must match the offline screen's picks: glm-5.2-led coding mix
	// with fable-5 on the rest.
	knobs := s.defaultActiveKnobs()
	require.Len(t, knobs.Alpha, 12, "default_routing_knobs.alpha must be a length-12 vector")
	for i, a := range knobs.Alpha {
		assert.InDeltaf(t, 0.7, a, 1e-9, "cluster %d alpha must be the 0.7 sweet spot", i)
	}

	require.Len(t, s.models, 21, "all 21 models must be eligible under the full provider set")

	wins := map[string]int{}
	for c := 0; c < bundle.Centroids.K; c++ {
		scores := s.blendScoresV2([]int{c}, knobs, s.models, nil, nil)
		winner, _ := argmax(scores, s.models)
		require.NotEmptyf(t, winner, "cluster %d must have a non-empty argmax winner", c)
		wins[winner]++
	}

	// The bake-off candidate is intentionally a glm-5.2 + fable-5 coding mix.
	assert.Equal(t, 8, wins["z-ai/glm-5.2"], "glm-5.2 must lead 8 clusters at alpha=0.7")
	assert.Equal(t, 4, wins["claude-fable-5"], "fable-5 must lead 4 clusters at alpha=0.7")
	// No opus/haiku/legacy-sonnet cluster wins — the whole point of the candidate.
	assert.Zero(t, wins["claude-opus-4-8"], "no cluster should route to opus-4-8 at the screen alpha")
	assert.Zero(t, wins["claude-haiku-4-5"], "no cluster should route to haiku at the screen alpha")
}
