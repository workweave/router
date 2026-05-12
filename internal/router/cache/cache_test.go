package cache_test

import (
	"math"
	"net/http"
	"testing"
	"time"

	"workweave/router/internal/router/cache"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// l2Normalize returns vec / ||vec||₂; matches the cluster scorer's
// embedder output shape.
func l2Normalize(v []float32) []float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	norm := float32(math.Sqrt(float64(sum)))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x / norm
	}
	return out
}

// blendVectors returns L2-normalized alpha*a + (1-alpha)*b — used to
// generate near-duplicates with tunable cosine.
func blendVectors(a, b []float32, alpha float32) []float32 {
	out := make([]float32, len(a))
	for i := range a {
		out[i] = alpha*a[i] + (1-alpha)*b[i]
	}
	return l2Normalize(out)
}

func sampleResponse(body string) cache.CachedResponse {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return cache.CachedResponse{
		StatusCode: http.StatusOK,
		Headers:    h,
		Body:       []byte(body),
	}
}

func TestCache_IdenticalEmbeddingHits(t *testing.T) {
	c := cache.New(cache.DefaultConfig())
	emb := l2Normalize([]float32{1, 0, 0, 0})
	want := sampleResponse(`{"id":"resp-1"}`)

	c.Store("inst-1", cache.FormatAnthropic, emb, 0, want)

	got, hit := c.Lookup("inst-1", cache.FormatAnthropic, emb, []int{0, 1, 2, 3})
	require.True(t, hit, "identical embedding should hit")
	assert.Equal(t, want.Body, got.Body)
	assert.Equal(t, want.StatusCode, got.StatusCode)
}

func TestCache_NearDuplicateHitsAboveThreshold(t *testing.T) {
	cfg := cache.DefaultConfig()
	cfg.DefaultThreshold = 0.95
	c := cache.New(cfg)

	a := l2Normalize([]float32{1, 0, 0, 0})
	// Near-duplicate at cosine ≈ 0.97.
	b := blendVectors(a, l2Normalize([]float32{0, 1, 0, 0}), 0.95)

	var sim float32
	for i := range a {
		sim += a[i] * b[i]
	}
	require.Greater(t, sim, float32(0.95), "test fixture cosine must be above the threshold")

	c.Store("inst-1", cache.FormatAnthropic, a, 0, sampleResponse(`{"id":"resp-near"}`))

	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, b, []int{0})
	assert.True(t, hit, "near-duplicate above threshold should hit")
}

func TestCache_BelowThresholdMisses(t *testing.T) {
	cfg := cache.DefaultConfig()
	cfg.DefaultThreshold = 0.95
	c := cache.New(cfg)

	a := l2Normalize([]float32{1, 0, 0, 0})
	// Distant — cosine ~0.5.
	b := blendVectors(a, l2Normalize([]float32{0, 1, 0, 0}), 0.5)

	c.Store("inst-1", cache.FormatAnthropic, a, 0, sampleResponse(`{"id":"resp"}`))

	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, b, []int{0})
	assert.False(t, hit, "below-threshold cosine must miss")
}

func TestCache_PerClusterThresholdOverride(t *testing.T) {
	cfg := cache.DefaultConfig()
	cfg.DefaultThreshold = 0.99 // strict
	cfg.PerClusterThreshold = map[int]float32{
		7: 0.5, // permissive for cluster 7 only
	}
	c := cache.New(cfg)

	a := l2Normalize([]float32{1, 0, 0, 0})
	b := blendVectors(a, l2Normalize([]float32{0, 1, 0, 0}), 0.7) // cosine ≈ 0.7

	c.Store("inst-1", cache.FormatAnthropic, a, 0, sampleResponse(`{"id":"strict"}`))
	_, hitStrict := c.Lookup("inst-1", cache.FormatAnthropic, b, []int{0})
	assert.False(t, hitStrict, "default 0.99 threshold should reject 0.7 cosine")

	c.Store("inst-1", cache.FormatAnthropic, a, 7, sampleResponse(`{"id":"loose"}`))
	resp, hitLoose := c.Lookup("inst-1", cache.FormatAnthropic, b, []int{7})
	require.True(t, hitLoose, "cluster 7 override 0.5 threshold should accept 0.7 cosine")
	assert.Equal(t, []byte(`{"id":"loose"}`), resp.Body)
}

func TestCache_BucketIsolationAcrossInstallations(t *testing.T) {
	c := cache.New(cache.DefaultConfig())
	emb := l2Normalize([]float32{1, 0, 0, 0})

	c.Store("inst-A", cache.FormatAnthropic, emb, 0, sampleResponse(`{"who":"A"}`))

	_, hit := c.Lookup("inst-B", cache.FormatAnthropic, emb, []int{0})
	assert.False(t, hit, "embeddings must not cross installations")
}

func TestCache_BucketIsolationAcrossFormats(t *testing.T) {
	c := cache.New(cache.DefaultConfig())
	emb := l2Normalize([]float32{1, 0, 0, 0})

	c.Store("inst-1", cache.FormatAnthropic, emb, 0, sampleResponse(`{"fmt":"anth"}`))

	_, hit := c.Lookup("inst-1", cache.FormatOpenAI, emb, []int{0})
	assert.False(t, hit, "cached Anthropic response must not replay for OpenAI")
}

func TestCache_TTLExpiry(t *testing.T) {
	cfg := cache.DefaultConfig()
	cfg.TTL = 50 * time.Millisecond
	c := cache.New(cfg)

	emb := l2Normalize([]float32{1, 0, 0, 0})
	c.Store("inst-1", cache.FormatAnthropic, emb, 0, sampleResponse(`{"id":"old"}`))

	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, emb, []int{0})
	require.True(t, hit, "fresh entry should hit")

	time.Sleep(80 * time.Millisecond)
	_, hit = c.Lookup("inst-1", cache.FormatAnthropic, emb, []int{0})
	assert.False(t, hit, "expired entry must miss")
}

func TestCache_LookupScansAllTopPClusters(t *testing.T) {
	c := cache.New(cache.DefaultConfig())
	emb := l2Normalize([]float32{1, 0, 0, 0})

	// Store in cluster 5; top-p lookup must find it via the scan.
	c.Store("inst-1", cache.FormatAnthropic, emb, 5, sampleResponse(`{"id":"in-5"}`))

	got, hit := c.Lookup("inst-1", cache.FormatAnthropic, emb, []int{2, 3, 5, 7})
	require.True(t, hit, "top-p sweep should locate entries in any listed cluster")
	assert.Equal(t, []byte(`{"id":"in-5"}`), got.Body)
}

func TestCache_StoreDropsOversizedBodies(t *testing.T) {
	cfg := cache.DefaultConfig()
	cfg.MaxBodyBytes = 16
	c := cache.New(cfg)

	emb := l2Normalize([]float32{1, 0, 0, 0})
	bigBody := make([]byte, cfg.MaxBodyBytes+1)
	c.Store("inst-1", cache.FormatAnthropic, emb, 0, cache.CachedResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{},
		Body:       bigBody,
	})

	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, emb, []int{0})
	assert.False(t, hit, "oversized bodies must not be stored")
}

func TestCache_NilCacheLookupAndStoreAreSafe(t *testing.T) {
	// Disabled-mode: callers pass nil and expect no-op.
	var c *cache.Cache

	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, []float32{1}, []int{0})
	assert.False(t, hit, "nil cache must report a miss without panicking")

	c.Store("inst-1", cache.FormatAnthropic, []float32{1}, 0, sampleResponse(`x`))
}

func TestCache_EmptyEmbeddingMisses(t *testing.T) {
	c := cache.New(cache.DefaultConfig())
	_, hit := c.Lookup("inst-1", cache.FormatAnthropic, nil, []int{0})
	assert.False(t, hit, "empty embedding must miss")
	c.Store("inst-1", cache.FormatAnthropic, nil, 0, sampleResponse(`x`))
	_, hit = c.Lookup("inst-1", cache.FormatAnthropic, l2Normalize([]float32{1, 0, 0, 0}), []int{0})
	assert.False(t, hit, "empty-embedding store must be a no-op")
}
