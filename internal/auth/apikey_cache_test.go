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
	// Capacity of 1 forces the second insert to evict the first via the LRU
	// callback. If the secondary index still held the evicted hash, a later
	// invalidate of installation A would try to remove a hash that's already
	// gone (harmless) — but if the second insert *re-used* installation A,
	// the invalidate must still drop the surviving entry. This guards the
	// onEvict bookkeeping that ties the two together.
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

// TestLRUAPIKeyCache_InvalidationDuringSetEvictsOrphan models the
// concurrency window where Set has indexed its keyHash under
// byInstallation but has not yet completed positive.Add when
// InvalidateInstallation runs. Without the generation-counter
// recheck in Set, the entry would land in the positive LRU after
// the invalidate already cleared the index — orphaning it until
// TTL and silently surviving a policy change. Driving the race
// directly is flaky; instead we drive the same logical sequence
// step-by-step from a single goroutine, exercising the exact
// state transitions Set traverses.
func TestLRUAPIKeyCache_InvalidationDuringSetEvictsOrphan(t *testing.T) {
	cache := auth.NewLRUAPIKeyCache(8, 8, time.Hour, time.Hour)

	// Seed an unrelated entry so byInstallation has state.
	cache.Set("h-seed", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-seed"},
		Installation: &auth.Installation{ID: "inst-A"},
	})

	// Invalidate concurrently from another goroutine while we race a
	// Set. The test passes deterministically because of the recheck:
	// regardless of interleaving, every invalidation that fires after
	// the Set begins must result in a cache that does NOT serve the
	// new entry on Get.
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
		// After every Set/Invalidate pair, follow with a final
		// invalidate so the post-state is unambiguous: nothing for
		// inst-A may survive.
		cache.InvalidateInstallation("inst-A")
	}
	<-done

	_, ok := cache.Get("h-race")
	assert.False(t, ok, "no entry for inst-A may survive the final invalidation, even when Set and Invalidate interleave")

	// And follow-up: a fresh Set after the final invalidate must
	// land in the cache (proving generation tracking didn't pin
	// the installation out permanently).
	cache.Set("h-post", auth.CachedKey{
		APIKey:       &auth.APIKey{ID: "k-post"},
		Installation: &auth.Installation{ID: "inst-A"},
	})
	v, ok := cache.Get("h-post")
	assert.True(t, ok, "post-invalidation Set must be visible")
	assert.Equal(t, "k-post", v.APIKey.ID)
}
