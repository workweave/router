package subscriptions

import (
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
)

const (
	poolCacheSize = 4096
	poolCacheTTL  = 2 * time.Minute
)

// poolCache is a short-TTL read-through cache of a user's full active pool
// (both providers), keyed by installation+email. It exists to keep the
// hot-path exhaustion/enrollment checks off the DB; the TTL bounds staleness
// after an enroll/remove on another replica (no cross-replica invalidation in
// this cut — see the package plan). Local mutations evict directly.
type poolCache struct {
	entries *lru.LRU[string, []*Credential]
}

func newPoolCache() *poolCache {
	return &poolCache{
		entries: lru.NewLRU[string, []*Credential](poolCacheSize, nil, poolCacheTTL),
	}
}

func (c *poolCache) get(installationID, userEmail string) ([]*Credential, bool) {
	return c.entries.Get(poolCacheKey(installationID, userEmail))
}

func (c *poolCache) set(installationID, userEmail string, pool []*Credential) {
	c.entries.Add(poolCacheKey(installationID, userEmail), pool)
}

func (c *poolCache) evict(installationID, userEmail string) {
	c.entries.Remove(poolCacheKey(installationID, userEmail))
}

func poolCacheKey(installationID, userEmail string) string {
	return installationID + "\x00" + userEmail
}
