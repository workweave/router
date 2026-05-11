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

// fakeEmbedder returns a fixed vector or error. Captures the last
// argument so tests can assert tail-truncation happened upstream.
type fakeEmbedder struct {
	vec      []float32
	err      error
	lastText string
	calls    int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.calls++
	f.lastText = text
	return f.vec, f.err
}

// l2norm normalizes v in place. Centroids and embeddings are L2-normed
// at training/embed time so the scorer's dot product is cosine
// similarity. Test fixtures honor that contract.
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

// float32Sqrt is the obvious thing; using it instead of math.Sqrt avoids
// a float64 round-trip for fixture math (the production path uses
// Float32 throughout).
func float32Sqrt(x float32) float32 {
	guess := x / 2
	for i := 0; i < 5; i++ {
		guess = 0.5 * (guess + x/guess)
	}
	return guess
}

// bundleFromBlobs runs the real loaders against caller-built blobs and
// returns a *Bundle the scorer can be constructed against.
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

// twoClusterArtifacts builds a minimal artifact set with K=2 distinct
// centroids in EmbedDim space. Cluster 0 is the +e1 direction; cluster
// 1 is the +e2 direction. Cluster 0 prefers Opus; cluster 1 prefers
// Haiku.
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

// allProviders is the test-default availableProviders set.
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
	// K=2 in test fixtures; default TopP=4 would be > K. Tighten.
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

// TestScorer_PopulatesRoutingMetadata asserts the scorer surfaces
// candidate set, chosen score, and artifact version so the proxy can
// record routing observations without reaching into private scorer state.
// Removing any of these breaks routing telemetry rows downstream.
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

// TestScorer_ReturnsErrOnShortPrompt: the scorer fails loud rather than
// silently degrading to a default model — silent fallback masked real
// regressions in eval.
func TestScorer_ReturnsErrOnShortPrompt(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newScorerForTest(t, emb, cfgForTest())

	_, err := s.Route(context.Background(), router.Request{PromptText: "hi"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrClusterUnavailable))
	assert.Equal(t, 0, emb.calls, "embedder should not be called for short prompts")
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
	// We keep the *tail*; the suffix of the input must be the suffix of
	// what reached the embedder.
	assert.True(t, strings.HasSuffix(prompt, emb.lastText), "tail-truncate must preserve suffix")
}

func TestScorer_TopPSumsAcrossClusters(t *testing.T) {
	// Build a 3-cluster artifact where cluster 2 has overwhelming Haiku
	// preference; with TopP=2 (clusters 0+1 nearest), Opus wins; with
	// TopP=3, Haiku takes over once cluster 2's row is summed in.
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

	// Only Anthropic registered: must pick Anthropic despite gpt-5 having a higher score.
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"anthropic": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
	assert.Equal(t, "anthropic", got.Provider)

	// Both Anthropic and OpenAI registered: gpt-5 wins on score.
	s, err = NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()},
		map[string]struct{}{"anthropic": {}, "openai": {}})
	require.NoError(t, err)
	got, err = s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "openai", got.Provider)
	assert.Contains(t, got.Reason, "provider=openai")
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

// TestScorer_ReturnsErrOnEmbedTimeout proves the per-request EmbedTimeout
// causes ErrClusterUnavailable rather than a silent heuristic fallback.
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

// twoProviderArtifacts builds a 2-cluster fixture with one candidate per
// provider (anthropic / openai), where the openai model would otherwise
// outscore the anthropic model on the cluster the prompt aligns to. Used
// to exercise per-request EnabledProviders gating.
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

// TestScorer_NilEnabledProvidersPreservesBootBehavior is the regression
// guard for the gating opt-in: when the proxy passes no per-request
// provider set, argmax runs unrestricted over the boot-time candidates.
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

// TestScorer_EnabledProvidersGatesArgmax is the load-bearing assertion:
// even when the unrestricted argmax would pick openai, restricting
// EnabledProviders to {anthropic} forces argmax onto anthropic candidates.
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

// TestScorer_EmptyEnabledProvidersReturnsErrNoEligibleProvider asserts
// the typed error: an installation with no resolvable provider keys
// must surface a 4xx-mappable error rather than picking a model the
// upstream call would 401 on.
func TestScorer_EmptyEnabledProvidersReturnsErrNoEligibleProvider(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	s := newTwoProviderScorer(t, emb)

	_, err := s.Route(context.Background(), router.Request{
		PromptText:       strings.Repeat("x", 100),
		EnabledProviders: map[string]struct{}{"google": {}},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNoEligibleProvider))
	// Must NOT also surface as ErrClusterUnavailable; the API layer
	// maps these two sentinels to different status codes (400 vs 503).
	assert.False(t, errors.Is(err, ErrClusterUnavailable))
}
