package auth

import (
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// CachedKey is the value stored in APIKeyCache. Negative=true marks a known-bad token; APIKey/Installation are nil then.
type CachedKey struct {
	APIKey       *APIKey
	Installation *Installation
	ExternalKeys []*ExternalAPIKey
	Negative     bool
}

// APIKeyCache is an in-process read-through cache for the auth lookup. Must be safe for concurrent use.
// Invalidation is TTL-only: per-instance caches are independent and a soft-deleted key remains usable
// until expiry. Acceptable given the manual rotation flow.
type APIKeyCache interface {
	Get(keyHash string) (CachedKey, bool)
	Set(keyHash string, entry CachedKey)
}

// NoOpAPIKeyCache is the Null Object: every Get misses, every Set is dropped.
type NoOpAPIKeyCache struct{}

func (NoOpAPIKeyCache) Get(string) (CachedKey, bool) { return CachedKey{}, false }
func (NoOpAPIKeyCache) Set(string, CachedKey)        {}

// LRUAPIKeyCache uses two LRUs so positive/negative entries have independent sizes and TTLs.
// Negative gets a shorter TTL so a freshly-created key isn't 401'd longer than necessary.
type LRUAPIKeyCache struct {
	positive *expirable.LRU[string, CachedKey]
	negative *expirable.LRU[string, CachedKey]
}

func NewLRUAPIKeyCache(positiveSize, negativeSize int, positiveTTL, negativeTTL time.Duration) *LRUAPIKeyCache {
	return &LRUAPIKeyCache{
		positive: expirable.NewLRU[string, CachedKey](positiveSize, nil, positiveTTL),
		negative: expirable.NewLRU[string, CachedKey](negativeSize, nil, negativeTTL),
	}
}

func (c *LRUAPIKeyCache) Get(keyHash string) (CachedKey, bool) {
	if v, ok := c.positive.Get(keyHash); ok {
		return v, true
	}
	if v, ok := c.negative.Get(keyHash); ok {
		return v, true
	}
	return CachedKey{}, false
}

func (c *LRUAPIKeyCache) Set(keyHash string, entry CachedKey) {
	if entry.Negative {
		c.negative.Add(keyHash, entry)
	} else {
		c.positive.Add(keyHash, entry)
	}
}
