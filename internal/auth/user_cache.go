package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// UserCache memoizes resolved router user IDs keyed on (installation_id, email)
// so the hot request path doesn't UPSERT on every call. Cache hits skip the DB
// entirely; the trade-off is that last_seen_at lags by up to the cache TTL,
// which is acceptable for a dashboard timestamp.
type UserCache interface {
	Get(installationID, email string) (string, bool)
	Set(installationID, email, userID string)
}

// NoOpUserCache is the Null Object — every Get misses, every Set is dropped.
type NoOpUserCache struct{}

func (NoOpUserCache) Get(string, string) (string, bool) { return "", false }
func (NoOpUserCache) Set(string, string, string)        {}

// LRUUserCache wraps an expirable LRU. Keys are sha256(installation_id || email)
// hex-encoded so the in-memory map doesn't pin user emails as plain strings any
// longer than necessary.
type LRUUserCache struct {
	store *expirable.LRU[string, string]
}

func NewLRUUserCache(size int, ttl time.Duration) *LRUUserCache {
	return &LRUUserCache{
		store: expirable.NewLRU[string, string](size, nil, ttl),
	}
}

func (c *LRUUserCache) Get(installationID, email string) (string, bool) {
	return c.store.Get(userCacheKey(installationID, email))
}

func (c *LRUUserCache) Set(installationID, email, userID string) {
	c.store.Add(userCacheKey(installationID, email), userID)
}

func userCacheKey(installationID, email string) string {
	h := sha256.New()
	h.Write([]byte(installationID))
	h.Write([]byte{0x00})
	h.Write([]byte(email))
	return hex.EncodeToString(h.Sum(nil))
}
