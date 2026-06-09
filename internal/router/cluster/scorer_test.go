package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/router"
)

// fakeEmbedder returns a fixed vector or error; captures last text so
// tests can assert tail-truncation happened upstream. Zero-value id/dim
// default to the Jina identity so legacy-shaped fixtures keep working.
type fakeEmbedder struct {
	vec      []float32
	err      error
	id       string
	dim      int
	lastText string
	calls    int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	f.lastText = text
	return f.vec, f.err
}

func (f *fakeEmbedder) ID() string {
	if f.id == "" {
		return EmbedderJinaV2
	}
	return f.id
}

func (f *fakeEmbedder) Dim() int {
	if f.dim == 0 {
		return EmbedDim
	}
	return f.dim
}

// l2norm normalizes v in place; test fixtures honor the L2-normed
// contract so dot product is cosine similarity.
func l2norm(v []float32) {
	var s float32
	for _, x := range v {
		s += x * x
	}
	if s == 0 {
		return
	}
	inv := 1.0 / float32Sqrt(s)
	for i := range v {
		v[i] *= inv
	}
}

// float32Sqrt avoids the float64 round-trip of math.Sqrt; production
// path uses float32 throughout.
func float32Sqrt(x float32) float32 {
	guess := x / 2
	for i := 0; i < 5; i++ {
		guess = 0.5 * (guess + x/guess)
	}
	return guess
}

// bundleFromBlobs runs real loaders against caller-built blobs.
func bundleFromBlobs(t *testing.T, version string, centroidsBlob, rankingsBlob, registryBlob []byte) *Bundle {
	t.Helper()
	c, err := loadCentroids(centroidsBlob)
	require.NoError(t, err)
	r, err := loadRankings(rankingsBlob)
	require.NoError(t, err)
	reg, err := loadRegistry(registryBlob)
	require.NoError(t, err)
	return &Bundle{
		Version:   version,
		Centroids: c,
		Rankings:  r,
		Registry:  reg,
	}
}

// twoClusterArtifacts: K=2 fixture. Cluster 0 (+e1) prefers Opus;
// cluster 1 (+e2) prefers Haiku.
func twoClusterArtifacts(t *testing.T) (centroidsBlob, rankingsBlob, registryBlob []byte) {
	t.Helper()
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	full := append(append([]float32{}, c0...), c1...)
	centroidsBlob = buildCentroidsBlob(t, 2, dim, full)

	rankingsBlob = []byte(`{
		"rankings": {
			"0": {"claude-opus-4-7": 0.9, "claude-haiku-4-5": 0.1},
			"1": {"claude-opus-4-7": 0.1, "claude-haiku-4-5": 0.9}
		}
	}`)
	registryBlob = []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	return
}

func allProviders() map[string]struct{} {
	return map[string]struct{}{
		"anthropic": {},
		"openai":    {},
		"google":    {},
	}
}

func makeOpusVec() []float32 {
	v := make([]float32, EmbedDim)
	v[0] = 1 // aligned with cluster 0 (Opus)
	return v
}

func makeHaikuVec() []float32 {
	v := make([]float32, EmbedDim)
	v[1] = 1 // aligned with cluster 1 (Haiku)
	return v
}

func newScorerForTest(t *testing.T, embedder Embedder, cfg Config) *Scorer {
	t.Helper()
	cb, rb, regb := twoClusterArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	s, err := NewScorer(bundle, cfg, embedder, allProviders())
	require.NoError(t, err)
	return s
}

func cfgForTest() Config {
	c := DefaultConfig()
	// K=2 fixtures; default TopP=4 > K. Tighten.
	c.TopP = 1
	return c
}

func TestScorer_PicksClusterAlignedModel(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newScorerForTest(t, emb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", got.Model)
	assert.Equal(t, "anthropic", got.Provider)
	assert.Contains(t, got.Reason, "cluster:v-test top_p=[0]")
	assert.Contains(t, got.Reason, "model=claude-opus-4-7")
}

// Removing any populated metadata field breaks routing telemetry rows.
func TestScorer_PopulatesRoutingMetadata(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newScorerForTest(t, emb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
	})
	require.NoError(t, err)
	require.NotNil(t, got.Metadata, "scorer must populate Metadata for cluster-routed decisions")
	assert.Equal(t, "v-test", got.Metadata.ClusterRouterVersion)
	assert.ElementsMatch(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, got.Metadata.CandidateModels,
		"candidate_models must mirror the eligible argmax set")
	assert.NotZero(t, got.Metadata.ChosenScore, "chosen_score must be non-zero for a real decision")
	assert.Equal(t, []int{0}, got.Metadata.ClusterIDs,
		"with cfgForTest TopP=1, only the closest cluster (Opus-aligned) is summed")
}

func TestScorer_PicksOtherClusterWhenAligned(t *testing.T) {
	emb := &fakeEmbedder{vec: makeHaikuVec()}
	s := newScorerForTest(t, emb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("y", 100),
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
}

// imageFilterArtifacts: K=1 fixture whose single cluster ranks the text-only
// z-ai/glm-5.1 above the image-capable claude-opus-4-7. Without an image in the
// request the scorer picks glm-5.1; with one it must drop glm-5.1 and fall to
// opus.
func imageFilterArtifacts(t *testing.T) (centroidsBlob, rankingsBlob, registryBlob []byte) {
	t.Helper()
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	centroidsBlob = buildCentroidsBlob(t, 1, dim, c0)
	rankingsBlob = []byte(`{
		"rankings": {
			"0": {"z-ai/glm-5.1": 0.9, "claude-opus-4-7": 0.1}
		}
	}`)
	registryBlob = []byte(`{
		"deployed_models": [
			{"model": "z-ai/glm-5.1", "provider": "deepinfra", "bench_column": "x", "proxy": true},
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "y", "proxy": true}
		]
	}`)
	return
}

func TestScorer_DropsTextOnlyModelOnImageTurn(t *testing.T) {
	cb, rb, regb := imageFilterArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	available := map[string]struct{}{"anthropic": {}, "deepinfra": {}}
	s, err := NewScorer(bundle, cfgForTest(), &fakeEmbedder{vec: makeOpusVec()}, available)
	require.NoError(t, err)

	// No image: the higher-ranked text-only model wins.
	textTurn, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "z-ai/glm-5.1", textTurn.Model, "text turn routes to the cluster-preferred model")

	// Image present: glm-5.1 is dropped, opus is the only image-capable candidate.
	imageTurn, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100), HasImages: true})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", imageTurn.Model, "image turn must skip the text-only model")
	assert.NotContains(t, imageTurn.Metadata.CandidateModels, "z-ai/glm-5.1",
		"text-only model must be absent from the image-turn candidate set")
}

func TestScorer_KeepsTextOnlyPoolWhenNoImageCapableCandidate(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {"z-ai/glm-5.1": 0.9}}}`)
	regb := []byte(`{"deployed_models": [{"model": "z-ai/glm-5.1", "provider": "deepinfra", "bench_column": "x", "proxy": true}]}`)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	s, err := NewScorer(bundle, cfgForTest(), &fakeEmbedder{vec: makeOpusVec()}, map[string]struct{}{"deepinfra": {}})
	require.NoError(t, err)

	// Soft fallback: no image-capable candidate deployed, so the scorer still
	// returns a decision rather than erroring — the upstream reports the
	// rejection instead.
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100), HasImages: true})
	require.NoError(t, err)
	assert.Equal(t, "z-ai/glm-5.1", got.Model)
}

func TestScorer_ReturnsErrOnEmbedderError(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("ort exploded")}
	s := newScorerForTest(t, emb, cfgForTest())

	_, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClusterUnavailable))
}

func TestScorer_ReturnsErrOnDimMismatch(t *testing.T) {
	emb := &fakeEmbedder{vec: make([]float32, 7)} // wrong size
	s := newScorerForTest(t, emb, cfgForTest())

	_, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClusterUnavailable))
}

func TestScorer_TailTruncatesBeforeEmbed(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	cfg := cfgForTest()
	cfg.MaxPromptChars = 32

	s := newScorerForTest(t, emb, cfg)

	prompt := strings.Repeat("HEAD-", 50) + "TAIL_END"
	_, err := s.Route(context.Background(), router.Request{PromptText: prompt})
	require.NoError(t, err)
	require.LessOrEqual(t, len(emb.lastText), cfg.MaxPromptChars)
	assert.True(t, strings.HasSuffix(prompt, emb.lastText), "tail-truncate must preserve suffix")
}

func TestScorer_TopPSumsAcrossClusters(t *testing.T) {
	// 3 clusters; cluster 2 has overwhelming Haiku preference. TopP=2
	// → Opus wins; TopP=3 → Haiku wins once cluster 2 is summed.
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	c2 := make([]float32, dim)
	c2[2] = 1
	full := append(append(append([]float32{}, c0...), c1...), c2...)
	centroidsBlob := buildCentroidsBlob(t, 3, dim, full)

	rankingsBlob := []byte(`{
		"rankings": {
			"0": {"claude-opus-4-7": 0.6, "claude-haiku-4-5": 0.0},
			"1": {"claude-opus-4-7": 0.6, "claude-haiku-4-5": 0.0},
			"2": {"claude-opus-4-7": 0.0, "claude-haiku-4-5": 5.0}
		}
	}`)
	registryBlob := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)

	vec := make([]float32, dim)
	vec[0] = 1
	vec[1] = 1
	vec[2] = 1
	l2norm(vec)
	emb := &fakeEmbedder{vec: vec}

	for _, tc := range []struct {
		topP    int
		want    string
		comment string
	}{
		{1, "claude-opus-4-7", "top-1 lands on cluster 0 (sorted-ascending tie-break), prefers Opus"},
		{2, "claude-opus-4-7", "top-2 sums clusters 0+1, both prefer Opus"},
		{3, "claude-haiku-4-5", "top-3 includes cluster 2 with 5.0 Haiku score; sum overwhelms 1.2 Opus"},
	} {
		t.Run(tc.comment, func(t *testing.T) {
			cfg := cfgForTest()
			cfg.TopP = tc.topP
			emb.calls = 0
			bundle := bundleFromBlobs(t, "v-test", centroidsBlob, rankingsBlob, registryBlob)
			s, err := NewScorer(bundle, cfg, emb, allProviders())
			require.NoError(t, err)
			got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("z", 100)})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.Model)
		})
	}
}

func TestNewScorer_RejectsTopPExceedingK(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t) // K=2
	cfg := cfgForTest()
	cfg.TopP = 5
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TopP")
}

func TestNewScorer_RejectsNilBundle(t *testing.T) {
	_, err := NewScorer(nil, cfgForTest(), &fakeEmbedder{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle")
}

func TestNewScorer_RejectsNilEmbedder(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), nil, allProviders())
	require.Error(t, err)
}

func TestNewScorer_RejectsEmptyAvailableProviders(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), &fakeEmbedder{}, map[string]struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "availableProviders")
}

// The identity guard: a bundle trained in one embedding space must never
// be scored with a different embedder, even at a matching dim.
func TestNewScorer_RejectsEmbedderIDMismatch(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	// Legacy bundle (no metadata) declares Jina; a Qwen embedder at the
	// same dim must be refused.
	_, err := NewScorer(bundle, cfgForTest(), &fakeEmbedder{id: EmbedderQwen3, dim: EmbedDim}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares embedder")
}

func TestNewScorer_RejectsEmbedderDimMismatch(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	_, err := NewScorer(bundle, cfgForTest(), &fakeEmbedder{dim: 1024}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "centroids dim")
}

// A 1024d Qwen bundle loads and routes end-to-end with a matching
// embedder — the dual-embedder path is not Jina-special-cased.
func TestScorer_QwenBundleRoutes(t *testing.T) {
	dim := 1024
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	full := append(append([]float32{}, c0...), c1...)
	cb := buildCentroidsBlob(t, 2, dim, full)
	rb := []byte(`{
		"rankings": {
			"0": {"claude-opus-4-7": 0.9, "claude-haiku-4-5": 0.1},
			"1": {"claude-opus-4-7": 0.1, "claude-haiku-4-5": 0.9}
		}
	}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	bundle := bundleFromBlobs(t, "v-qwen-test", cb, rb, regb)
	bundle.Metadata = &ArtifactMetadata{
		Embedder: ArtifactEmbedder{Model: EmbedderQwen3, EmbedDim: dim},
	}

	vec := make([]float32, dim)
	vec[0] = 1 // aligned with cluster 0 (Opus)
	emb := &fakeEmbedder{id: EmbedderQwen3, dim: dim, vec: vec}
	s, err := NewScorer(bundle, cfgForTest(), emb, allProviders())
	require.NoError(t, err)

	d, err := s.Route(context.Background(), router.Request{PromptText: "design a distributed system"})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", d.Model)
	assert.Equal(t, "anthropic", d.Provider)
}

func TestNewScorer_RejectsRankingsMissingDeployedModel(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {"claude-opus-4-7": 1.0}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing model")
}

func TestScorer_BootFailsWhenNoCandidatesMatchProviders(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), &fakeEmbedder{},
		map[string]struct{}{"openai": {}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no deployed entry matches")
}

func TestScorer_FiltersOutUnregisteredProvider(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"claude-haiku-4-5": 0.1,
		"gpt-5": 0.9,
		"gemini-2.5-pro": 0.5
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true},
			{"model": "gpt-5", "provider": "openai", "bench_column": "gpt-5"},
			{"model": "gemini-2.5-pro", "provider": "google", "bench_column": "gemini-2.5-pro"}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1

	// Anthropic only: must pick Anthropic despite gpt-5 scoring higher.
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"anthropic": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
	assert.Equal(t, "anthropic", got.Provider)

	// Anthropic + OpenAI: gpt-5 wins on score.
	s, err = NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"anthropic": {}, "openai": {}})
	require.NoError(t, err)
	got, err = s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "openai", got.Provider)
	assert.Contains(t, got.Reason, "provider=openai")
}

// Multi-binding regression: kimi-k2.5 has catalog bindings [bedrock,
// openrouter] (primary, fallback). A self-hoster wiring only OpenRouter
// must still route to kimi-k2.5 — the scorer should resolve via the
// trailing binding and emit Decision.Provider="openrouter", not drop the
// row because the registry's Provider field says "bedrock".
func TestScorer_MultiBindingResolvesFallbackProviderAtBoot(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"moonshotai/kimi-k2.5": 0.9,
		"claude-haiku-4-5": 0.1
	}}}`)
	// Registry row's provider is "bedrock" (the SOC 2-isolated primary).
	regb := []byte(`{
		"deployed_models": [
			{"model": "moonshotai/kimi-k2.5", "provider": "bedrock", "bench_column": "routerarena_moonshotai/kimi-k2.5"},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "routerarena_claude-haiku-4-5"}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1

	// Self-hoster: only OpenRouter wired. The catalog's openrouter fallback
	// binding must keep kimi-k2.5 in the candidate set.
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"openrouter": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "moonshotai/kimi-k2.5", got.Model)
	assert.Equal(t, "openrouter", got.Provider, "self-hoster should route via the trailing OpenRouter binding")
	assert.Contains(t, got.Reason, "provider=openrouter")
}

// Multi-binding regression: when both primary and fallback are wired,
// the primary (first in catalog order) wins. Verifies catalog's ordered
// fallback list semantics.
func TestScorer_MultiBindingPrefersPrimaryWhenBothAvailable(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"moonshotai/kimi-k2.5": 0.9,
		"claude-haiku-4-5": 0.1
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "moonshotai/kimi-k2.5", "provider": "bedrock", "bench_column": "routerarena_moonshotai/kimi-k2.5"},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "routerarena_claude-haiku-4-5"}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1

	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"bedrock": {}, "openrouter": {}, "anthropic": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "moonshotai/kimi-k2.5", got.Model)
	assert.Equal(t, "bedrock", got.Provider, "with both providers wired, the primary binding wins")
}

// Per-request EnabledProviders re-resolves the binding: a deploy with
// both bedrock + openrouter wired can still serve a per-request BYOK
// constraint of openrouter-only by walking down the fallback list.
func TestScorer_MultiBindingPerRequestResolvesNarrowedSet(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"moonshotai/kimi-k2.5": 0.9,
		"claude-haiku-4-5": 0.1
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "moonshotai/kimi-k2.5", "provider": "bedrock", "bench_column": "routerarena_moonshotai/kimi-k2.5"},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "routerarena_claude-haiku-4-5"}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1

	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"bedrock": {}, "openrouter": {}, "anthropic": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{
		PromptText:       strings.Repeat("x", 100),
		EnabledProviders: map[string]struct{}{"openrouter": {}},
	})
	require.NoError(t, err)
	assert.Equal(t, "moonshotai/kimi-k2.5", got.Model)
	assert.Equal(t, "openrouter", got.Provider, "per-request EnabledProviders={openrouter} must re-resolve to the openrouter fallback binding")
}

func TestScorer_DedupesDuplicateRegistryEntries(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"claude-opus-4-7": 0.35,
		"claude-haiku-4-5": 0.6
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "claude-opus-4-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()}, allProviders())
	require.NoError(t, err)

	counts := make(map[string]int, len(s.models))
	for _, m := range s.models {
		counts[m]++
	}
	for m, n := range counts {
		assert.Equalf(t, 1, n, "model %q appears %d times in s.models — duplicate registry entries must dedupe", m, n)
	}
	assert.ElementsMatch(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, s.models)

	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model, "haiku should win at 0.6 vs opus 0.35; if opus wins, the scoring loop is double-counting its two registry entries")
}

// Per-request EmbedTimeout must cause ErrClusterUnavailable, not a
// silent fallback.
func TestScorer_ReturnsErrOnEmbedTimeout(t *testing.T) {
	slow := &slowEmbedder{delay: 100 * time.Millisecond, vec: makeOpusVec()}
	cfg := cfgForTest()
	cfg.EmbedTimeout = 10 * time.Millisecond
	s := newScorerForTest(t, slow, cfg)

	_, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClusterUnavailable))
}

type slowEmbedder struct {
	vec   []float32
	delay time.Duration
}

func (s *slowEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	select {
	case <-time.After(s.delay):
		return s.vec, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *slowEmbedder) ID() string { return EmbedderJinaV2 }

func (s *slowEmbedder) Dim() int { return EmbedDim }

func TestTailTruncate(t *testing.T) {
	got := tailTruncate("abcdef", 3)
	assert.Equal(t, "def", got)

	assert.Equal(t, "ab", tailTruncate("ab", 5))

	in := "héllo"
	got = tailTruncate(in, 4)
	assert.True(t, len(got) <= 4)
	assert.True(t, strings.HasSuffix(in, got))
	for _, r := range got {
		assert.NotEqual(t, '�', r, "result must be valid UTF-8")
	}
}

func TestTopPNearest_DeterministicOnTies(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[0] = 1
	full := append(append([]float32{}, c0...), c1...)
	c, err := loadCentroids(buildCentroidsBlob(t, 2, dim, full))
	require.NoError(t, err)

	v := make([]float32, dim)
	v[0] = 1
	got := topPNearest(v, c, 1)
	assert.Equal(t, []int{0}, got)
}

func TestArgmax_TiebreakUsesOrderSlice(t *testing.T) {
	scores := map[string]float32{"A": 1.0, "B": 1.0}
	gotA, _ := argmax(scores, []string{"A", "B"})
	gotB, _ := argmax(scores, []string{"B", "A"})
	assert.Equal(t, "A", gotA)
	assert.Equal(t, "B", gotB)
}

// twoProviderArtifacts: 2 clusters, one candidate per provider. OpenAI
// outscores Anthropic on the aligned cluster — exercises per-request
// EnabledProviders gating.
func twoProviderArtifacts(t *testing.T) (centroidsBlob, rankingsBlob, registryBlob []byte) {
	t.Helper()
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	full := append(append([]float32{}, c0...), c1...)
	centroidsBlob = buildCentroidsBlob(t, 2, dim, full)

	rankingsBlob = []byte(`{
		"rankings": {
			"0": {"claude-opus-4-7": 0.5, "gpt-5": 0.9},
			"1": {"claude-opus-4-7": 0.1, "gpt-5": 0.05}
		}
	}`)
	registryBlob = []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "gpt-5", "provider": "openai", "bench_column": "gpt-5"}
		]
	}`)
	return
}

func newTwoProviderScorer(t *testing.T, emb Embedder) *Scorer {
	t.Helper()
	cb, rb, regb := twoProviderArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test-2p", cb, rb, regb)
	available := map[string]struct{}{"anthropic": {}, "openai": {}}
	s, err := NewScorer(bundle, cfgForTest(), emb, available)
	require.NoError(t, err)
	return s
}

// Regression guard for gating opt-in: nil EnabledProviders → argmax
// runs unrestricted over boot-time candidates.
func TestScorer_NilEnabledProvidersPreservesBootBehavior(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
	})
	require.NoError(t, err)
	// Cluster 0 ranks gpt-5 above opus; without gating, gpt-5 wins.
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "openai", got.Provider)
}

// Load-bearing: restricting EnabledProviders to {anthropic} forces
// argmax onto anthropic even when openai would otherwise win.
func TestScorer_EnabledProvidersGatesArgmax(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	got, err := s.Route(context.Background(), router.Request{
		PromptText:       strings.Repeat("x", 100),
		EnabledProviders: map[string]struct{}{"anthropic": {}},
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", got.Model)
	assert.Equal(t, "anthropic", got.Provider)
}

// Installation with no resolvable provider keys must surface a
// 4xx-mappable error, not pick a model that 401s upstream.
func TestScorer_EmptyEnabledProvidersReturnsErrNoEligibleProvider(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	_, err := s.Route(context.Background(), router.Request{
		PromptText:       strings.Repeat("x", 100),
		EnabledProviders: map[string]struct{}{"google": {}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoEligibleProvider))
	// Must not also surface as ErrClusterUnavailable; API maps these
	// sentinels to different status codes (400 vs 503).
	assert.False(t, errors.Is(err, ErrClusterUnavailable))
}

// Per-installation model exclusion drops the named model from argmax even
// when its provider is otherwise eligible.
func TestScorer_ExcludedModelsRemovesFromArgmax(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	got, err := s.Route(context.Background(), router.Request{
		PromptText:     strings.Repeat("x", 100),
		ExcludedModels: map[string]struct{}{"gpt-5": {}},
	})
	require.NoError(t, err)
	// Without exclusion gpt-5 wins; excluding it forces argmax onto claude.
	assert.Equal(t, "claude-opus-4-7", got.Model)
	assert.Equal(t, "anthropic", got.Provider)
}

// Excluding every deployed model must surface ErrNoEligibleProvider (no
// silent fallback to a default).
func TestScorer_ExcludedModelsEmptyingPoolReturnsErrNoEligibleProvider(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	_, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
		ExcludedModels: map[string]struct{}{
			"gpt-5":           {},
			"claude-opus-4-7": {},
		},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoEligibleProvider))
	assert.False(t, errors.Is(err, ErrClusterUnavailable))
}

// has_tools=true must subtract catalog.ToolUseLowSet from the argmax pool
// so qwen3-235b-Instruct (and other instruct-only weak tool callers) cannot
// be picked for agentic workloads.
func TestScorer_HasToolsExcludesToolUseLowFromArgmax(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	// Ranking puts qwen3-235b above claude-opus so without the filter it
	// would win; the filter must demote it on has_tools=true.
	rb := []byte(`{"rankings": {"0": {
		"qwen/qwen3-235b-a22b-2507": 0.95,
		"claude-opus-4-7": 0.10
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "qwen/qwen3-235b-a22b-2507", "provider": "bedrock", "bench_column": "routerarena_qwen/qwen3-235b-a22b-2507"},
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1
	s, err := NewScorer(bundleFromBlobs(t, "v-test-toolfilter", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"bedrock": {}, "anthropic": {}})
	require.NoError(t, err)

	// No tools: qwen3-235b wins (highest score).
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "qwen/qwen3-235b-a22b-2507", got.Model, "without tools, qwen3-235b should win on score")

	// With tools: filter drops qwen3-235b → claude-opus wins.
	got, err = s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100), HasTools: true})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", got.Model, "has_tools=true must drop ToolUseLow models from argmax")
}

// Soft-filter regression: if the ToolUseLow filter would empty the eligible
// pool, fall back to the unfiltered set rather than 4xx-ing — this is a
// quality preference, not a correctness gate.
func TestScorer_HasToolsFallsBackWhenFilterEmptiesPool(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	rb := []byte(`{"rankings": {"0": {
		"qwen/qwen3-235b-a22b-2507": 0.95
	}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "qwen/qwen3-235b-a22b-2507", "provider": "bedrock", "bench_column": "routerarena_qwen/qwen3-235b-a22b-2507"}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1
	s, err := NewScorer(bundleFromBlobs(t, "v-test-fallback", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"bedrock": {}})
	require.NoError(t, err)

	// has_tools=true: filter would empty the pool, so we keep qwen3-235b
	// rather than returning an error.
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100), HasTools: true})
	require.NoError(t, err)
	assert.Equal(t, "qwen/qwen3-235b-a22b-2507", got.Model, "filter must fall back when it would empty the pool")
}

// DeployedModels returns the full provider-filtered candidate list, not the
// per-request eligible subset.
func TestScorer_DeployedModelsReturnsBootCandidates(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	got := s.DeployedModels()
	models := make([]string, 0, len(got))
	for _, e := range got {
		models = append(models, e.Model)
	}
	// Two-provider scorer fixture has gpt-5 + claude-opus-4-7.
	assert.Contains(t, models, "gpt-5")
	assert.Contains(t, models, "claude-opus-4-7")
}

func TestScorer_V2DynamicScoring(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	regBlob := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	full := append(append([]float32{}, c0...), c1...)
	centroidsBlob := buildCentroidsBlob(t, 2, dim, full)

	centroids, err := loadCentroids(centroidsBlob)
	require.NoError(t, err)
	registry, err := loadRegistry(regBlob)
	require.NoError(t, err)

	qualityMeans := Rankings{
		0: map[string]float32{"claude-opus-4-7": 0.9, "claude-haiku-4-5": 0.1},
		1: map[string]float32{"claude-opus-4-7": 0.1, "claude-haiku-4-5": 0.9},
	}

	opusInputPrice := 0.015
	opusOutputPrice := 0.075
	haikuInputPrice := 0.00025
	haikuOutputPrice := 0.00125

	opusTTFT := 0.8
	opusTPS := 60.0
	haikuTTFT := 0.2
	haikuTPS := 120.0

	opusVerbosity := 4000.0
	haikuVerbosity := 1000.0

	modelAxes := map[string]ModelAxis{
		"claude-opus-4-7": {
			InputPer1KUSD:   &opusInputPrice,
			OutputPer1KUSD:  &opusOutputPrice,
			TTFTSeconds:     &opusTTFT,
			TPS:             &opusTPS,
			VerbosityTokens: &opusVerbosity,
		},
		"claude-haiku-4-5": {
			InputPer1KUSD:   &haikuInputPrice,
			OutputPer1KUSD:  &haikuOutputPrice,
			TTFTSeconds:     &haikuTTFT,
			TPS:             &haikuTPS,
			VerbosityTokens: &haikuVerbosity,
		},
	}

	meta := &ArtifactMetadata{
		FormatVersion: 2,
		Training: ArtifactTraining{
			K:    2,
			TopP: 2,
			DefaultRoutingKnobs: &DefaultRoutingKnobs{
				Alpha:                []float64{0.5, 0.5},
				SpeedWeight:          0.0,
				OutputCostRatio:      0.0,
				ExpectedOutputTokens: 2000,
				PerModelVerbosity:    false,
			},
		},
	}

	bundle := &Bundle{
		Version:         "v2-test",
		Centroids:       centroids,
		Registry:        registry,
		Metadata:        meta,
		IsV2:            true,
		QualityMeans:    qualityMeans,
		ModelAxes:       modelAxes,
		MedianVerbosity: 2500.0,
	}

	cfg := DefaultConfig()
	cfg.TopP = 1
	scorer, err := NewScorer(bundle, cfg, emb, allProviders())
	require.NoError(t, err)

	// Test case 1: Default knobs (alpha=0.5, speed_weight=0, output_cost_ratio=0).
	dec, err := scorer.Route(context.Background(), router.Request{
		RequestedModel: "auto",
		PromptText:     "test",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, dec.Model)
	assert.NotNil(t, dec.Metadata)
	assert.NotZero(t, dec.Metadata.EffectiveKnobsHash)

	// Test case 2: Override alpha to 1.0 (extreme quality-preferring).
	alphaVal := 1.0
	decQuality, err := scorer.Route(context.Background(), router.Request{
		RequestedModel: "auto",
		PromptText:     "test",
		RoutingKnobs: &router.Overrides{
			Alpha: &alphaVal,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", decQuality.Model)

	// Test case 3: Override alpha to 0.0 (extreme cost-preferring).
	alphaValZero := 0.0
	decCost, err := scorer.Route(context.Background(), router.Request{
		RequestedModel: "auto",
		PromptText:     "test",
		RoutingKnobs: &router.Overrides{
			Alpha: &alphaValZero,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", decCost.Model)

	// Test case 4: Out-of-bounds validation.
	badAlpha := -0.1
	_, err = scorer.Route(context.Background(), router.Request{
		RequestedModel: "auto",
		PromptText:     "test",
		RoutingKnobs: &router.Overrides{
			Alpha: &badAlpha,
		},
	})
	assert.ErrorIs(t, err, ErrInvalidRoutingKnobs)
}

// v2BundleOpts is the optional knob configuration for newV2BundleForTest.
type v2BundleOpts struct {
	qualityMeans    Rankings // overrides the default opus/haiku table
	modelAxes       map[string]ModelAxis
	medianVerbosity float64
	defaultKnobs    *DefaultRoutingKnobs
}

// newV2BundleForTest builds a synthetic two-cluster v2 bundle that
// matches twoClusterArtifacts (Opus + Haiku across clusters 0 and 1),
// with per-axis overrides via opts. Returns a Scorer wired against
// fakeEmbedder.
func newV2BundleForTest(t *testing.T, emb Embedder, opts v2BundleOpts) *Scorer {
	t.Helper()
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	c1 := make([]float32, dim)
	c1[1] = 1
	full := append(append([]float32{}, c0...), c1...)
	centroidsBlob := buildCentroidsBlob(t, 2, dim, full)
	centroids, err := loadCentroids(centroidsBlob)
	require.NoError(t, err)

	registry, err := loadRegistry([]byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`))
	require.NoError(t, err)

	qm := opts.qualityMeans
	if qm == nil {
		qm = Rankings{
			0: {"claude-opus-4-7": 0.9, "claude-haiku-4-5": 0.1},
			1: {"claude-opus-4-7": 0.1, "claude-haiku-4-5": 0.9},
		}
	}

	axes := opts.modelAxes
	if axes == nil {
		opusInputP := 0.015
		opusOutputP := 0.075
		haikuInputP := 0.00025
		haikuOutputP := 0.00125
		opusTTFT := 0.8
		opusTPS := 60.0
		haikuTTFT := 0.2
		haikuTPS := 120.0
		axes = map[string]ModelAxis{
			"claude-opus-4-7": {
				InputPer1KUSD:  &opusInputP,
				OutputPer1KUSD: &opusOutputP,
				TTFTSeconds:    &opusTTFT,
				TPS:            &opusTPS,
			},
			"claude-haiku-4-5": {
				InputPer1KUSD:  &haikuInputP,
				OutputPer1KUSD: &haikuOutputP,
				TTFTSeconds:    &haikuTTFT,
				TPS:            &haikuTPS,
			},
		}
	}

	median := opts.medianVerbosity
	if median == 0 {
		median = 1.0
	}

	defaults := opts.defaultKnobs
	if defaults == nil {
		defaults = &DefaultRoutingKnobs{
			Alpha:                []float64{0.5, 0.5},
			SpeedWeight:          0.0,
			OutputCostRatio:      0.0,
			ExpectedOutputTokens: 2000,
			PerModelVerbosity:    false,
		}
	}

	meta := &ArtifactMetadata{
		FormatVersion: 2,
		Training: ArtifactTraining{
			K:                   2,
			TopP:                2,
			DefaultRoutingKnobs: defaults,
		},
	}

	bundle := &Bundle{
		Version:         "v2-test",
		Centroids:       centroids,
		Registry:        registry,
		Metadata:        meta,
		IsV2:            true,
		QualityMeans:    qm,
		ModelAxes:       axes,
		MedianVerbosity: median,
	}

	cfg := DefaultConfig()
	cfg.TopP = 1
	scorer, err := NewScorer(bundle, cfg, emb, allProviders())
	require.NoError(t, err)
	return scorer
}

// TestDegenerateRangeFallthrough confirms that when every model ties on
// quality in a cluster (q_range==0), the blend collapses to cost (+ speed
// when enabled) only — matching the trainer's behavior at line 597/623.
func TestDegenerateRangeFallthrough(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	tied := Rankings{
		0: {"claude-opus-4-7": 0.5, "claude-haiku-4-5": 0.5},
		1: {"claude-opus-4-7": 0.5, "claude-haiku-4-5": 0.5},
	}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{
		qualityMeans: tied,
		defaultKnobs: &DefaultRoutingKnobs{
			Alpha:                []float64{0.5, 0.5},
			SpeedWeight:          0.0,
			OutputCostRatio:      1.0, // cost matters
			ExpectedOutputTokens: 2000,
		},
	})
	dec, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)
	// With quality tied and cost-aware blend, Haiku (cheaper) must win.
	assert.Equal(t, "claude-haiku-4-5", dec.Model, "tied quality should let cost decide")
}

// TestSpeedRangeZeroFallsBackToTwoAxis covers the case where exactly one
// deployed model has AA timing data, so s_range==0. The blend must fall
// back to (quality + cost) with w_s redistributed.
func TestSpeedRangeZeroFallsBackToTwoAxis(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	opusInputP := 0.015
	opusOutputP := 0.075
	haikuInputP := 0.00025
	haikuOutputP := 0.00125
	opusTTFT := 0.8
	opusTPS := 60.0
	axes := map[string]ModelAxis{
		"claude-opus-4-7": {
			InputPer1KUSD:  &opusInputP,
			OutputPer1KUSD: &opusOutputP,
			TTFTSeconds:    &opusTTFT,
			TPS:            &opusTPS,
		},
		"claude-haiku-4-5": {
			// No AA timing data — sPtr will be nil.
			InputPer1KUSD:  &haikuInputP,
			OutputPer1KUSD: &haikuOutputP,
		},
	}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{
		modelAxes: axes,
		defaultKnobs: &DefaultRoutingKnobs{
			Alpha:                []float64{0.5, 0.5},
			SpeedWeight:          0.3,
			OutputCostRatio:      0.0,
			ExpectedOutputTokens: 2000,
		},
	})
	dec, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)
	// With s_range==0, both models hit redistribution; Opus wins on
	// quality (cluster 0, opus=0.9 vs haiku=0.1).
	assert.Equal(t, "claude-opus-4-7", dec.Model, "s_range==0 must not crash; quality still decides")
}

// TestKnobOverrideOnlyReweights verifies that supplying explicit
// overrides equal to the bundle defaults yields the same decision as
// not supplying overrides — proves overrides reweight, they don't
// silently change anything else.
func TestKnobOverrideOnlyReweights(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{})

	decDefault, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)

	a := 0.5
	w := 0.0
	r := 0.0
	tk := 2000
	v := false
	decOverride, err := scorer.Route(context.Background(), router.Request{
		PromptText: "test",
		RoutingKnobs: &router.Overrides{
			Alpha:                &a,
			SpeedWeight:          &w,
			OutputCostRatio:      &r,
			ExpectedOutputTokens: &tk,
			PerModelVerbosity:    &v,
		},
	})
	require.NoError(t, err)

	assert.Equal(t, decDefault.Model, decOverride.Model, "override matching defaults must yield identical decision")
	assert.Equal(t, decDefault.Metadata.EffectiveKnobsHash, decOverride.Metadata.EffectiveKnobsHash, "matching knobs must hash identically")
}

// TestAlphaScalarReplacesVector confirms the sledgehammer behavior:
// a scalar alpha override uniformly overwrites every per-cluster alpha,
// discarding calibration. Set distinct per-cluster alphas and verify
// they're gone after override.
func TestAlphaScalarReplacesVector(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}

	// Per-cluster calibrated alphas: cluster 0 cares about quality more.
	calibrated := &DefaultRoutingKnobs{
		Alpha:                []float64{0.9, 0.1},
		SpeedWeight:          0.0,
		OutputCostRatio:      1.0, // cost active
		ExpectedOutputTokens: 2000,
	}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{defaultKnobs: calibrated})

	// At default knobs, vec aligned with cluster 0, alpha[0]=0.9 → quality dominates → Opus.
	decDefault, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", decDefault.Model, "alpha[0]=0.9 + cluster 0 should pick Opus")

	// Override alpha=0.0 (scalar). Both clusters get alpha=0 → cost dominates → Haiku.
	zero := 0.0
	decOverride, err := scorer.Route(context.Background(), router.Request{
		PromptText:   "test",
		RoutingKnobs: &router.Overrides{Alpha: &zero},
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", decOverride.Model, "scalar alpha=0 must overwrite per-cluster 0.9, letting cost decide")
}

// TestZeroAATimingFallback covers the case where w_s > 0 but no model
// has AA timing data. The blend should fall to (quality + cost) across
// the board with w_s proportionally redistributed.
func TestZeroAATimingFallback(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	opusInputP := 0.015
	opusOutputP := 0.075
	haikuInputP := 0.00025
	haikuOutputP := 0.00125
	axes := map[string]ModelAxis{
		"claude-opus-4-7": {
			InputPer1KUSD:  &opusInputP,
			OutputPer1KUSD: &opusOutputP,
			// No TTFT or TPS.
		},
		"claude-haiku-4-5": {
			InputPer1KUSD:  &haikuInputP,
			OutputPer1KUSD: &haikuOutputP,
		},
	}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{
		modelAxes: axes,
		defaultKnobs: &DefaultRoutingKnobs{
			Alpha:                []float64{0.5, 0.5},
			SpeedWeight:          0.5, // active, but no model has timing
			OutputCostRatio:      0.0,
			ExpectedOutputTokens: 2000,
		},
	})
	dec, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)
	// No timing data, w_s redistributed; quality dominates redistribution → Opus.
	assert.Equal(t, "claude-opus-4-7", dec.Model)
}

// TestPureSpeedMissingTimingFallsBackToQuality covers the extreme
// configuration where alpha=0, w_s=1 (pure speed). For a model with no
// AA timing, total = alpha + (1-alpha-w_s) = 0, so the fallback returns
// the raw qNorm — preventing a NaN or panic.
func TestPureSpeedMissingTimingFallsBackToQuality(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	haikuInputP := 0.00025
	haikuOutputP := 0.00125
	opusInputP := 0.015
	opusOutputP := 0.075
	axes := map[string]ModelAxis{
		"claude-opus-4-7": {
			// No AA timing data.
			InputPer1KUSD:  &opusInputP,
			OutputPer1KUSD: &opusOutputP,
		},
		"claude-haiku-4-5": {
			// No AA timing data.
			InputPer1KUSD:  &haikuInputP,
			OutputPer1KUSD: &haikuOutputP,
		},
	}
	scorer := newV2BundleForTest(t, emb, v2BundleOpts{
		modelAxes: axes,
		defaultKnobs: &DefaultRoutingKnobs{
			Alpha:                []float64{0.0, 0.0},
			SpeedWeight:          1.0,
			OutputCostRatio:      0.0,
			ExpectedOutputTokens: 2000,
		},
	})
	dec, err := scorer.Route(context.Background(), router.Request{PromptText: "test"})
	require.NoError(t, err)
	// With alpha=0, w_s=1, total fallback kicks in; raw qNorm wins. Opus
	// has qNorm=1 in cluster 0; Haiku has 0. Opus must win.
	assert.Equal(t, "claude-opus-4-7", dec.Model)
}

// TestInvalidEffectiveKnobs covers the validation paths that must return
// ErrInvalidRoutingKnobs.
func TestInvalidEffectiveKnobs(t *testing.T) {
	cases := []struct {
		name  string
		knobs router.Overrides
	}{
		{
			name: "alpha out of range",
			knobs: func() router.Overrides {
				a := 1.5
				return router.Overrides{Alpha: &a}
			}(),
		},
		{
			name: "alpha+speed_weight exceeds 1",
			knobs: func() router.Overrides {
				a := 0.8
				w := 0.5
				return router.Overrides{Alpha: &a, SpeedWeight: &w}
			}(),
		},
		{
			name: "speed_weight out of range",
			knobs: func() router.Overrides {
				w := 1.5
				return router.Overrides{SpeedWeight: &w}
			}(),
		},
		{
			name: "output_cost_ratio out of range",
			knobs: func() router.Overrides {
				r := 11.0
				return router.Overrides{OutputCostRatio: &r}
			}(),
		},
		{
			name: "expected_output_tokens out of range",
			knobs: func() router.Overrides {
				tk := 200000
				return router.Overrides{ExpectedOutputTokens: &tk}
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			emb := &fakeEmbedder{vec: makeOpusVec()}
			scorer := newV2BundleForTest(t, emb, v2BundleOpts{})
			_, err := scorer.Route(context.Background(), router.Request{
				PromptText:   "test",
				RoutingKnobs: &tc.knobs,
			})
			assert.ErrorIs(t, err, ErrInvalidRoutingKnobs, "%s must return ErrInvalidRoutingKnobs", tc.name)
		})
	}
}
