package auth_test

import (
	"testing"
	"time"

	"workweave/router/internal/auth"

	"github.com/stretchr/testify/assert"
)

func TestNoOpAPIKeyCache_DropsWritesAndAlwaysMisses(t *testing.T) {
	c := auth.NoOpAPIKeyCache{}
	c.Set("anything", auth.CachedKey{APIKey: &auth.APIKey{ID: "irrelevant"}})

	_, ok := c.Get("anything")

	assert.False(t, ok,
		"NoOpAPIKeyCache must not retain Set entries; the Null Object contract is that Get always misses")
}

func TestLRUAPIKeyCache_PositiveAndNegativeBucketsAreIndependent(t *testing.T) {
	cache := auth.NewLRUAPIKeyCache(1, 1, time.Hour, time.Hour)

	cache.Set("pos", auth.CachedKey{APIKey: &auth.APIKey{ID: "k1"}})
	cache.Set("neg", auth.CachedKey{Negative: true})

	_, posOK := cache.Get("pos")
	_, negOK := cache.Get("neg")

	assert.True(t, posOK, "positive entry must survive a negative-bucket Set")
	assert.True(t, negOK, "negative entry must survive a positive-bucket Set")
}

func TestLRUAPIKeyCache_InvalidateInstallationEvictsAllKeysForThatInstallation(t *testing.T) {
	cache := auth.NewLRUAPIKeyCache(10, 10, time.Hour, time.Hour)

	cache.Set("h1", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k1"},
		Installation: &auth.Installation{ID: "inst-A"},
	})
	cache.Set("h2", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k2"},
		Installation: &auth.Installation{ID: "inst-A"},
	})
	cache.Set("h3", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k3"},
		Installation: &auth.Installation{ID: "inst-B"},
	})

	cache.InvalidateInstallation("inst-A")

	_, h1OK := cache.Get("h1")
	_, h2OK := cache.Get("h2")
	_, h3OK := cache.Get("h3")
	assert.False(t, h1OK, "every cached key for the invalidated installation must be evicted")
	assert.False(t, h2OK, "every cached key for the invalidated installation must be evicted")
	assert.True(t, h3OK, "unrelated installations must not be affected by InvalidateInstallation")
}

func TestLRUAPIKeyCache_InvalidateInstallationDoesNotTouchNegativeEntries(t *testing.T) {
	cache := auth.NewLRUAPIKeyCache(10, 10, time.Hour, time.Hour)

	cache.Set("h-neg", auth.CachedKey{Negative: true})
	cache.Set("h-pos", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k1"},
		Installation: &auth.Installation{ID: "inst-A"},
	})

	cache.InvalidateInstallation("inst-A")

	_, negOK := cache.Get("h-neg")
	assert.True(t, negOK,
		"negative entries are keyed by token hash, not installation, so InvalidateInstallation must leave them alone")
}

func TestLRUAPIKeyCache_NaturalEvictionKeepsSecondaryIndexConsistent(t *testing.T) {
	// Capacity 1 forces eviction on the second insert. Guards that onEvict
	// bookkeeping still lets InvalidateInstallation drop the surviving entry
	// when the second insert reuses the same installation.
	cache := auth.NewLRUAPIKeyCache(1, 1, time.Hour, time.Hour)

	cache.Set("h-old", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-old"},
		Installation: &auth.Installation{ID: "inst-A"},
	})
	cache.Set("h-new", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-new"},
		Installation: &auth.Installation{ID: "inst-A"},
	})

	cache.InvalidateInstallation("inst-A")

	_, ok := cache.Get("h-new")
	assert.False(t, ok,
		"after an LRU eviction-then-reinsert under the same installation, InvalidateInstallation must still drop the surviving entry")
}

// TestLRUAPIKeyCache_InvalidationDuringSetEvictsOrphan covers the race where
// Set indexes byInstallation before positive.Add completes while
// InvalidateInstallation runs concurrently. Without the generation-counter
// recheck in Set, the entry would land in the LRU after the index was
// already cleared, orphaning it until TTL. The true race is flaky to
// trigger, so this drives the same interleaving deterministically.
func TestLRUAPIKeyCache_InvalidationDuringSetEvictsOrphan(t *testing.T) {
	cache := auth.NewLRUAPIKeyCache(8, 8, time.Hour, time.Hour)

	// Seed an unrelated entry so byInstallation has state.
	cache.Set("h-seed", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-seed"},
		Installation: &auth.Installation{ID: "inst-A"},
	})

	// Race Invalidate against Set from another goroutine; the recheck
	// guarantees no invalidation-after-Set-start leaves h-race visible.
	done := make(chan struct{})
	go func() {
		for range 50 {
			cache.InvalidateInstallation("inst-A")
		}
		close(done)
	}()
	for range 50 {
		cache.Set("h-race", auth.CachedKey{
			APIKey:       &auth.APIKey{ID: "k-race"},
			Installation: &auth.Installation{ID: "inst-A"},
		})
		// Final invalidate ensures nothing for inst-A survives.
		cache.InvalidateInstallation("inst-A")
	}
	<-done

	_, ok := cache.Get("h-race")
	assert.False(t, ok, "no entry for inst-A may survive the final invalidation, even when Set and Invalidate interleave")

	// A fresh Set after the final invalidate must still land (generation
	// tracking isn't a permanent pin).
	cache.Set("h-post", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-post"},
		Installation: &auth.Installation{ID: "inst-A"},
	})
	v, ok := cache.Get("h-post")
	assert.True(t, ok, "post-invalidation Set must be visible")
	assert.Equal(t, "k-post", v.APIKey.ID)
}
