package auth

import (
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// CachedKey is the value stored in APIKeyCache. Negative marks a known-bad token.
type CachedKey struct {
	APIKey       *APIKey
	Installation *Installation
	ExternalKeys []*ExternalAPIKey
	Negative     bool
}

// APIKeyCache is an in-process read-through cache for the auth lookup.
// Positive entries can be invalidated by installation ID.
type APIKeyCache interface {
	Get(keyHash string) (CachedKey, bool)
	Set(keyHash string, entry CachedKey)
	InvalidateInstallation(installationID string)
}

// NoOpAPIKeyCache is the Null Object: every Get misses.
type NoOpAPIKeyCache struct{}

func (NoOpAPIKeyCache) Get(string) (CachedKey, bool)  { return CachedKey{}, false }
func (NoOpAPIKeyCache) Set(string, CachedKey)         {}
func (NoOpAPIKeyCache) InvalidateInstallation(string) {}

// LRUAPIKeyCache uses two LRUs for positive/negative entries with independent sizes and TTLs.
// Negative TTL is shorter so a freshly-created key isn't 401'd longer than necessary.
// byInstallation is a secondary index for O(keys-per-installation) eviction on settings changes.
// eviction callbacks keep the index in sync. Negative entries are not indexed.
type LRUAPIKeyCache struct {
	mu             sync.Mutex
	positive       *expirable.LRU[string, CachedKey]
	negative       *expirable.LRU[string, CachedKey]
	byInstallation map[string]map[string]struct{}
	// invalidationGen detects invalidation races between index update and LRU insert.
	// We cannot hold mu across positive.Add because the LRU eviction callback acquires mu,
	// which would deadlock when Add triggers a capacity eviction.
	invalidationGen map[string]uint64
}

func NewLRUAPIKeyCache(positiveSize, negativeSize int, positiveTTL, negativeTTL time.Duration) *LRUAPIKeyCache {
	c := &LRUAPIKeyCache{
		byInstallation:  make(map[string]map[string]struct{}),
		invalidationGen: make(map[string]uint64),
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
	instID := ""
	if entry.Installation != nil {
		instID = entry.Installation.ID
	}
	var preGen uint64
	c.mu.Lock()
	if instID != "" {
		preGen = c.invalidationGen[instID]
		hashes, ok := c.byInstallation[instID]
		if !ok {
			hashes = make(map[string]struct{}, 1)
			c.byInstallation[instID] = hashes
		}
		hashes[keyHash] = struct{}{}
	}
	c.mu.Unlock()
	c.positive.Add(keyHash, entry)
	if instID == "" {
		return
	}
	// Closes the race with InvalidateInstallation. If a concurrent
	// invalidation drained byInstallation[instID] between the index
	// update above and positive.Add, the generation counter has bumped;
	// the entry we just inserted is now orphaned (not tracked in
	// byInstallation), so evict it and roll back our index entry.
	c.mu.Lock()
	if c.invalidationGen[instID] != preGen {
		if hashes, ok := c.byInstallation[instID]; ok {
			delete(hashes, keyHash)
			if len(hashes) == 0 {
				delete(c.byInstallation, instID)
			}
		}
		c.mu.Unlock()
		c.positive.Remove(keyHash)
		return
	}
	c.mu.Unlock()
}

// InvalidateInstallation drops every positive entry for installationID so the next auth
// re-reads from Postgres. Negative entries (per-token) are untouched.
func (c *LRUAPIKeyCache) InvalidateInstallation(installationID string) {
	if installationID == "" {
		return
	}
	c.mu.Lock()
	hashes := c.byInstallation[installationID]
	delete(c.byInstallation, installationID)
	c.invalidationGen[installationID]++
	c.mu.Unlock()
	for hash := range hashes {
		c.positive.Remove(hash)
	}
}

// onPositiveEvict keeps byInstallation in sync with the LRU on eviction or TTL expiry.
// Must not hold c.mu when calling Remove (would deadlock with Add's capacity eviction).
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
