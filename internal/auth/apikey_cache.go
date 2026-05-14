package auth

import (
	"sync"
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
// Positive entries can be invalidated by installation ID via InvalidateInstallation so settings changes
// (excluded models, BYOK keys) are visible on the next request; the TTL is a safety net for replicas
// that miss the invalidation signal.
type APIKeyCache interface {
	Get(keyHash string) (CachedKey, bool)
	Set(keyHash string, entry CachedKey)
	InvalidateInstallation(installationID string)
}

// NoOpAPIKeyCache is the Null Object: every Get misses, every Set is dropped.
type NoOpAPIKeyCache struct{}

func (NoOpAPIKeyCache) Get(string) (CachedKey, bool)   { return CachedKey{}, false }
func (NoOpAPIKeyCache) Set(string, CachedKey)          {}
func (NoOpAPIKeyCache) InvalidateInstallation(string)  {}

// LRUAPIKeyCache uses two LRUs so positive/negative entries have independent sizes and TTLs.
// Negative gets a shorter TTL so a freshly-created key isn't 401'd longer than necessary.
//
// byInstallation is a secondary index over the positive LRU; it lets writers evict every
// cached key for an installation in O(keys-per-installation) when settings change. Eviction
// callbacks (LRU capacity churn or TTL expiry) keep the index in sync so it never accumulates
// dangling hashes. Negative entries are not indexed: they're keyed by hash and have no
// installation_id binding.
type LRUAPIKeyCache struct {
	mu             sync.Mutex
	positive       *expirable.LRU[string, CachedKey]
	negative       *expirable.LRU[string, CachedKey]
	byInstallation map[string]map[string]struct{}
}

func NewLRUAPIKeyCache(positiveSize, negativeSize int, positiveTTL, negativeTTL time.Duration) *LRUAPIKeyCache {
	c := &LRUAPIKeyCache{
		byInstallation: make(map[string]map[string]struct{}),
	}
	c.positive = expirable.NewLRU(positiveSize, c.onPositiveEvict, positiveTTL)
	c.negative = expirable.NewLRU[string, CachedKey](negativeSize, nil, negativeTTL)
	return c
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
		return
	}
	c.mu.Lock()
	if entry.Installation != nil && entry.Installation.ID != "" {
		hashes, ok := c.byInstallation[entry.Installation.ID]
		if !ok {
			hashes = make(map[string]struct{}, 1)
			c.byInstallation[entry.Installation.ID] = hashes
		}
		hashes[keyHash] = struct{}{}
	}
	c.mu.Unlock()
	c.positive.Add(keyHash, entry)
}

// InvalidateInstallation drops every positive entry tied to installationID so the next request
// for any of its API keys re-reads the row (with refreshed ExcludedModels / ExternalKeys) from
// Postgres. Negative entries are untouched: they're per-token and unaffected by installation
// settings.
func (c *LRUAPIKeyCache) InvalidateInstallation(installationID string) {
	if installationID == "" {
		return
	}
	c.mu.Lock()
	hashes := c.byInstallation[installationID]
	delete(c.byInstallation, installationID)
	c.mu.Unlock()
	for hash := range hashes {
		c.positive.Remove(hash)
	}
}

// onPositiveEvict keeps byInstallation consistent with the LRU. Fires for both capacity-driven
// evictions and TTL expiries, including the synchronous Remove calls made by InvalidateInstallation
// (which is why we must not hold c.mu when calling Remove).
func (c *LRUAPIKeyCache) onPositiveEvict(keyHash string, entry CachedKey) {
	if entry.Installation == nil || entry.Installation.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hashes, ok := c.byInstallation[entry.Installation.ID]
	if !ok {
		return
	}
	delete(hashes, keyHash)
	if len(hashes) == 0 {
		delete(c.byInstallation, entry.Installation.ID)
	}
}
