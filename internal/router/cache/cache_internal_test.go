package cache

import (
	"net/http"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBrokenCache builds a Cache bypassing New()'s validation, so cfg fields
// that would make lru.New fail (size <= 0) survive into bucket().
func newBrokenCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	installations, err := lru.New[string, *installationCache](1)
	require.NoError(t, err)
	return &Cache{
		cfg:           cfg,
		now:           time.Now,
		installations: installations,
	}
}

// TestBucket_InvalidMaxBucketsPerInstallationNoOpsInsteadOfPanic pins finding
// [32]: a size <= 0 passed to lru.New for a never-seen installation must be
// treated as a cache-miss/no-op, not a request-path panic.
func TestBucket_InvalidMaxBucketsPerInstallationNoOpsInsteadOfPanic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxBucketsPerInstallation = 0 // lru.New rejects size <= 0
	c := newBrokenCache(t, cfg)

	assert.NotPanics(t, func() {
		b := c.bucket("inst-1", FormatAnthropic, 0, "v1", 0, true)
		assert.Nil(t, b, "bucket allocation failure must surface as nil, not panic")
	})
}

// TestBucket_InvalidBucketSizeNoOpsInsteadOfPanic pins the second lru.New
// call site (per-bucket entry LRU) under the same rule.
func TestBucket_InvalidBucketSizeNoOpsInsteadOfPanic(t *testing.T) {
	cfg := DefaultConfig()
	cfg.BucketSize = 0 // lru.New rejects size <= 0
	c := newBrokenCache(t, cfg)

	assert.NotPanics(t, func() {
		b := c.bucket("inst-1", FormatAnthropic, 0, "v1", 0, true)
		assert.Nil(t, b, "bucket allocation failure must surface as nil, not panic")
	})
}

// TestLookupAndStore_SurviveBrokenBucketAllocation exercises the actual
// request-path entry points (Lookup/Store), not just bucket() directly, to
// prove callers never observe a panic when lru.New fails.
func TestLookupAndStore_SurviveBrokenBucketAllocation(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxBucketsPerInstallation = 0
	c := newBrokenCache(t, cfg)
	emb := []float32{1, 0, 0, 0}

	assert.NotPanics(t, func() {
		c.Store("inst-1", FormatAnthropic, emb, 0, CachedResponse{StatusCode: http.StatusOK}, "v1", 0)
	})

	var hit bool
	assert.NotPanics(t, func() {
		_, hit = c.Lookup("inst-1", FormatAnthropic, emb, []int{0}, "v1", 0)
	})
	assert.False(t, hit, "broken bucket allocation must degrade to a cache miss")
}
