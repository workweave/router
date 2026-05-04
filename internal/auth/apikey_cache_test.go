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
