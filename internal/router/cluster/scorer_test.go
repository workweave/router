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

// recordingFallback records every Route call so tests can assert
// fallback delegation.
type recordingFallback struct {
	decision router.Decision
	err      error
	calls    int
	lastReq  router.Request
}

func (r *recordingFallback) Route(_ context.Context, req router.Request) (router.Decision, error) {
	r.calls++
	r.lastReq = req
	return r.decision, r.err
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
	// Newton iteration — three steps is plenty for unit-norm test
	// vectors built from small ints.
	guess := x / 2
	for i := 0; i < 5; i++ {
		guess = 0.5 * (guess + x/guess)
	}
	return guess
}

// bundleFromBlobs runs the real loaders against caller-built blobs and
// returns a *Bundle the scorer can be constructed against. Replaces the
// old "swap package-level embed vars" hack — bundles are now first-class
// arguments so each test can assemble one without mutating package state.
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
// Haiku. This is the canonical fixture for "embedding near cluster X
// routes to its preferred model" tests.
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

// allProviders is the test-default availableProviders set. Most tests want
// every provider key registered so filtering doesn't drop fixture entries.
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

func newScorerForTest(t *testing.T, embedder Embedder, fallback router.Router, cfg Config) *Scorer {
	t.Helper()
	cb, rb, regb := twoClusterArtifacts(t)
	bundle := bundleFromBlobs(t, "v-test", cb, rb, regb)
	s, err := NewScorer(bundle, cfg, embedder, fallback, allProviders())
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
	fb := &recordingFallback{decision: router.Decision{Model: "fallback"}}
	s := newScorerForTest(t, emb, fb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("x", 100),
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", got.Model)
	assert.Equal(t, "anthropic", got.Provider)
	assert.Contains(t, got.Reason, "cluster:v-test top_p=[0]")
	assert.Contains(t, got.Reason, "model=claude-opus-4-7")
	assert.Equal(t, 0, fb.calls, "fallback should not run on the success path")
}

func TestScorer_PicksOtherClusterWhenAligned(t *testing.T) {
	emb := &fakeEmbedder{vec: makeHaikuVec()}
	fb := &recordingFallback{decision: router.Decision{Model: "fallback"}}
	s := newScorerForTest(t, emb, fb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{
		PromptText: strings.Repeat("y", 100),
	})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
}

func TestScorer_FallsBackOnShortPrompt(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	fbDec := router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "heuristic:short_prompt"}
	fb := &recordingFallback{decision: fbDec}
	s := newScorerForTest(t, emb, fb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{PromptText: "hi"})
	require.NoError(t, err)
	assert.Equal(t, fbDec, got)
	assert.Equal(t, 1, fb.calls)
	assert.Equal(t, 0, emb.calls, "embedder should not be called for short prompts")
}

func TestScorer_FallsBackOnEmbedderError(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("ort exploded")}
	fbDec := router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "heuristic:short_prompt"}
	fb := &recordingFallback{decision: fbDec}
	s := newScorerForTest(t, emb, fb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, fbDec, got)
	assert.Equal(t, 1, fb.calls)
}

func TestScorer_FallsBackOnDimMismatch(t *testing.T) {
	emb := &fakeEmbedder{vec: make([]float32, 7)} // wrong size
	fbDec := router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "heuristic:short_prompt"}
	fb := &recordingFallback{decision: fbDec}
	s := newScorerForTest(t, emb, fb, cfgForTest())

	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, fbDec, got)
}

func TestScorer_TailTruncatesBeforeEmbed(t *testing.T) {
	emb := &fakeEmbedder{vec: makeOpusVec()}
	fb := &recordingFallback{decision: router.Decision{Model: "fallback"}}
	cfg := cfgForTest()
	cfg.MaxPromptChars = 32

	s := newScorerForTest(t, emb, fb, cfg)

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

	// Embed: equally close to clusters 0, 1, 2 (third closest is the
	// cluster that ties with the others when normalized).
	vec := make([]float32, dim)
	vec[0] = 1
	vec[1] = 1
	vec[2] = 1
	l2norm(vec)
	emb := &fakeEmbedder{vec: vec}
	fb := &recordingFallback{decision: router.Decision{Model: "fallback"}}

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
			s, err := NewScorer(bundle, cfg, emb, fb, allProviders())
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
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{}, &recordingFallback{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "TopP")
}

func TestNewScorer_RejectsNilBundle(t *testing.T) {
	_, err := NewScorer(nil, cfgForTest(), &fakeEmbedder{}, &recordingFallback{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle")
}

func TestNewScorer_RejectsNilEmbedder(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), nil, &recordingFallback{}, allProviders())
	require.Error(t, err)
}

func TestNewScorer_RejectsNilFallback(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), &fakeEmbedder{}, nil, allProviders())
	require.Error(t, err)
}

func TestNewScorer_RejectsEmptyAvailableProviders(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), &fakeEmbedder{}, &recordingFallback{}, map[string]struct{}{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "availableProviders")
}

func TestNewScorer_RejectsRankingsMissingDeployedModel(t *testing.T) {
	dim := EmbedDim
	c0 := make([]float32, dim)
	c0[0] = 1
	cb := buildCentroidsBlob(t, 1, dim, c0)
	// Rankings only knows Opus, but registry advertises Haiku too.
	rb := []byte(`{"rankings": {"0": {"claude-opus-4-7": 1.0}}}`)
	regb := []byte(`{
		"deployed_models": [
			{"model": "claude-opus-4-7", "provider": "anthropic", "bench_column": "gpt-5", "proxy": true},
			{"model": "claude-haiku-4-5", "provider": "anthropic", "bench_column": "gemini-2.5-flash", "proxy": true}
		]
	}`)
	cfg := cfgForTest()
	cfg.TopP = 1
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{}, &recordingFallback{}, allProviders())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing model")
}

// TestScorer_FiltersByAvailableProviders proves that when an entry's provider
// is absent from availableProviders, argmax never selects it. With both
// Anthropic candidates filtered out (only "openai" registered, but the fixture
// has no openai entry), boot must error so misconfigured deployments fail loud.
func TestScorer_BootFailsWhenNoCandidatesMatchProviders(t *testing.T) {
	cb, rb, regb := twoClusterArtifacts(t)
	_, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfgForTest(), &fakeEmbedder{}, &recordingFallback{},
		map[string]struct{}{"openai": {}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no deployed entry matches")
}

// TestScorer_FiltersOutUnregisteredProvider proves that an entry whose
// provider is not registered is silently dropped from argmax candidates,
// even if it has the higher score. With a 3-entry fixture (one Anthropic, one
// OpenAI, one Google) and only "anthropic" registered, the Anthropic entry must
// win regardless of score.
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
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()}, &recordingFallback{},
		map[string]struct{}{"anthropic": {}})
	require.NoError(t, err)
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model)
	assert.Equal(t, "anthropic", got.Provider)

	// Both Anthropic and OpenAI registered: gpt-5 wins on score.
	s, err = NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()}, &recordingFallback{},
		map[string]struct{}{"anthropic": {}, "openai": {}})
	require.NoError(t, err)
	got, err = s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "gpt-5", got.Model)
	assert.Equal(t, "openai", got.Provider)
	assert.Contains(t, got.Reason, "provider=openai")
}

// TestScorer_DedupesDuplicateRegistryEntries proves the scoring loop
// iterates each model exactly once even when the registry lists the
// same model under multiple bench_column entries (proxy chains).
// Without dedup the scoring loop's `scores[m] += row[m]` accumulator
// would collapse on the model-name map key and effectively multiply
// the model's argmax weight by its registry-entry count.
//
// The fixture: one cluster, claude-opus-4-7 listed twice (gpt-5 +
// claude-opus-4-5 proxies, the actual v0.6+ pattern), claude-haiku-4-5
// once. Rankings give haiku=0.6 and opus=0.35. Without dedup, opus
// would accumulate to 0.70 (×2) and beat haiku's 0.6. With dedup,
// haiku's 0.6 wins.
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
	s, err := NewScorer(bundleFromBlobs(t, "v-test", cb, rb, regb), cfg, &fakeEmbedder{vec: makeOpusVec()}, &recordingFallback{}, allProviders())
	require.NoError(t, err)

	// Direct assertion: each model name appears exactly once in the
	// iteration list, regardless of how many registry entries it had.
	counts := make(map[string]int, len(s.models))
	for _, m := range s.models {
		counts[m]++
	}
	for m, n := range counts {
		assert.Equalf(t, 1, n, "model %q appears %d times in s.models — duplicate registry entries must dedupe", m, n)
	}
	assert.ElementsMatch(t, []string{"claude-opus-4-7", "claude-haiku-4-5"}, s.models)

	// End-to-end: argmax must pick haiku (0.6), not opus (which
	// would win at 0.7 if double-counted).
	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	assert.Equal(t, "claude-haiku-4-5", got.Model, "haiku should win at 0.6 vs opus 0.35; if opus wins, the scoring loop is double-counting its two registry entries")
}

// TestScorer_ContextCanceledDuringEmbed proves the per-request
// EmbedTimeout bounds the call. Uses a slow embedder + a short timeout.
func TestScorer_ContextCanceledDuringEmbed(t *testing.T) {
	slow := &slowEmbedder{delay: 100 * time.Millisecond, vec: makeOpusVec()}
	fbDec := router.Decision{Provider: "anthropic", Model: "claude-haiku-4-5", Reason: "heuristic:short_prompt"}
	fb := &recordingFallback{decision: fbDec}
	cfg := cfgForTest()
	cfg.EmbedTimeout = 10 * time.Millisecond
	s := newScorerForTest(t, slow, fb, cfg)

	got, err := s.Route(context.Background(), router.Request{PromptText: strings.Repeat("x", 100)})
	require.NoError(t, err)
	// Slow embedder honors ctx and returns its error; scorer falls open.
	assert.Equal(t, fbDec, got)
	assert.Equal(t, 1, fb.calls)
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
	// ASCII case: keeps the last n bytes verbatim.
	got := tailTruncate("abcdef", 3)
	assert.Equal(t, "def", got)

	// Short input is returned as-is.
	assert.Equal(t, "ab", tailTruncate("ab", 5))

	// UTF-8 boundary: cutting in the middle of "é" (0xC3 0xA9) must not
	// produce malformed output. We tail-truncate "héllo" (6 bytes:
	// 0x68 0xC3 0xA9 0x6C 0x6C 0x6F) to maxChars=4. Naive cut at byte 2
	// would give "\xA9llo"; the snap-forward should produce "llo".
	in := "héllo"
	got = tailTruncate(in, 4)
	assert.True(t, len(got) <= 4)
	assert.True(t, strings.HasSuffix(in, got))
	for _, r := range got {
		assert.NotEqual(t, '�', r, "result must be valid UTF-8")
	}
}

func TestTopPNearest_DeterministicOnTies(t *testing.T) {
	// Two centroids at the same distance from a vec — the lower index
	// must win deterministically so log replay is stable.
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
	// Tied scores between A and B: the model first in `order` wins.
	scores := map[string]float32{"A": 1.0, "B": 1.0}
	gotA, _ := argmax(scores, []string{"A", "B"})
	gotB, _ := argmax(scores, []string{"B", "A"})
	assert.Equal(t, "A", gotA)
	assert.Equal(t, "B", gotB)
}
