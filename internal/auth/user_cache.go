package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// UserCache memoizes resolved router user IDs keyed on (installation_id, identityKey) to skip the
// UPSERT on the hot path. identityKey is "email:<addr>" or "account:<uuid>" — the two key spaces are
// disjoint so an account-only request never false-hits an email-bearing row.
// Cache hits skip the DB; last_seen_at lags by up to the cache TTL.
type UserCache interface {
	Get(installationID, identityKey string) (string, bool)
	Set(installationID, identityKey, userID string)
}

// NoOpUserCache is the Null Object: every Get misses, every Set is dropped.
type NoOpUserCache struct{}

func (NoOpUserCache) Get(string, string) (string, bool) { return "", false }
func (NoOpUserCache) Set(string, string, string)        {}

// LRUUserCache wraps an expirable LRU. Keys are sha256(installation_id || identityKey) so the
// in-memory map doesn't pin user emails / account UUIDs as plain strings.
type LRUUserCache struct {
	store *expirable.LRU[string, string]
}

func NewLRUUserCache(size int, ttl time.Duration) *LRUUserCache {
	return &LRUUserCache{
		store: expirable.NewLRU[string, string](size, nil, ttl),
	}
}

func (c *LRUUserCache) Get(installationID, identityKey string) (string, bool) {
	return c.store.Get(userCacheKey(installationID, identityKey))
}

func (c *LRUUserCache) Set(installationID, identityKey, userID string) {
	c.store.Add(userCacheKey(installationID, identityKey), userID)
}

func userCacheKey(installationID, identityKey string) string {
	h := sha256.New()
	h.Write([]byte(installationID))
	h.Write([]byte{0x00})
	h.Write([]byte(identityKey))
	return hex.EncodeToString(h.Sum(nil))
}
