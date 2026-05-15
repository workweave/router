// Package cache implements a semantic response cache. Short-circuits
// near-duplicate non-streaming requests by cosine similarity on prompt
// embedding; per-(installation, inbound-format) isolation. TTL is the only
// eviction.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
)

// CachedResponse captures the upstream response in the inbound wire format.
type CachedResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte // Bounded by MaxBodyBytes; oversized entries are dropped.
}

// Config carries the runtime knobs.
type Config struct {
	PerClusterThreshold map[int]float32
	DefaultThreshold    float32
	// BucketSize caps per-(installation, format, clusterID) LRU size.
	BucketSize   int
	TTL          time.Duration
	MaxBodyBytes int
}

// DefaultConfig returns conservative defaults tuned for staging.
func DefaultConfig() Config {
	return Config{
		DefaultThreshold: 0.95,
		BucketSize:       1024,
		TTL:              1 * time.Hour,
		MaxBodyBytes:     1 << 20,
	}
}

// Cache is the in-memory semantic cache. Concurrent-safe.
type Cache struct {
	cfg     Config
	buckets map[bucketKey]*expirable.LRU[entryKey, *entry]
	mu      sync.Mutex
}

// New constructs a Cache; zero Config fields fall through to DefaultConfig.
func New(cfg Config) *Cache {
	def := DefaultConfig()
	if cfg.DefaultThreshold == 0 {
		cfg.DefaultThreshold = def.DefaultThreshold
	}
	if cfg.BucketSize <= 0 {
		cfg.BucketSize = def.BucketSize
	}
	if cfg.TTL <= 0 {
		cfg.TTL = def.TTL
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = def.MaxBodyBytes
	}
	return &Cache{
		cfg:     cfg,
		buckets: make(map[bucketKey]*expirable.LRU[entryKey, *entry]),
	}
}

// Format names the inbound wire format. Lookups must match storage so
// an Anthropic client never receives an OpenAI-shaped reply.
type Format string

const (
	FormatAnthropic Format = "anthropic"
	FormatOpenAI    Format = "openai"
)

type bucketKey struct {
	installationID string
	format         Format
	clusterID      int
}

// entryKey is a 16-byte sha256-truncated embedding digest, so we don't
// store the 3KB embedding twice as the LRU key.
type entryKey [16]byte

type entry struct {
	embedding []float32
	response  CachedResponse
}

// thresholdFor returns the cosine threshold for clusterID, falling
// through to the default.
func (c *Cache) thresholdFor(clusterID int) float32 {
	if t, ok := c.cfg.PerClusterThreshold[clusterID]; ok {
		return t
	}
	return c.cfg.DefaultThreshold
}

// Lookup walks buckets for (installation, format, clusterIDs) and
// returns the first entry whose cosine clears the threshold.
// embedding must be L2-normalized; clusterIDs order is irrelevant.
func (c *Cache) Lookup(installationID string, format Format, embedding []float32, clusterIDs []int) (CachedResponse, bool) {
	if c == nil || len(embedding) == 0 || len(clusterIDs) == 0 {
		return CachedResponse{}, false
	}
	for _, cid := range clusterIDs {
		bucket := c.bucket(installationID, format, cid, false)
		if bucket == nil {
			continue
		}
		threshold := c.thresholdFor(cid)
		// Keys() returns MRU-first, biasing toward most recently stored entry.
		for _, k := range bucket.Keys() {
			e, ok := bucket.Get(k)
			if !ok {
				continue
			}
			sim := cosine(embedding, e.embedding)
			if sim >= threshold {
				return e.response, true
			}
		}
	}
	return CachedResponse{}, false
}

// Store persists a response. clusterID should be one of the routing
// decision's top-p clusters. Oversized bodies are silently dropped.
func (c *Cache) Store(installationID string, format Format, embedding []float32, clusterID int, resp CachedResponse) {
	if c == nil || len(embedding) == 0 {
		return
	}
	if len(resp.Body) > c.cfg.MaxBodyBytes {
		return
	}
	bucket := c.bucket(installationID, format, clusterID, true)
	if bucket == nil {
		return
	}
	// Deep-copy: caller's slice may be mutated/recycled.
	embedCopy := make([]float32, len(embedding))
	copy(embedCopy, embedding)
	bucket.Add(entryKeyFor(embedCopy), &entry{
		embedding: embedCopy,
		response:  resp,
	})
}

// bucket returns the LRU for a key. create=false returns nil for missing
// buckets (lookup path); create=true allocates lazily.
func (c *Cache) bucket(installationID string, format Format, clusterID int, create bool) *expirable.LRU[entryKey, *entry] {
	key := bucketKey{installationID: installationID, format: format, clusterID: clusterID}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.buckets[key]
	if !ok {
		if !create {
			return nil
		}
		b = expirable.NewLRU[entryKey, *entry](c.cfg.BucketSize, nil, c.cfg.TTL)
		c.buckets[key] = b
	}
	return b
}

// entryKeyFor hashes embedding bytes so re-stores update in-place rather
// than churning the LRU.
func entryKeyFor(embedding []float32) entryKey {
	if len(embedding) == 0 {
		return entryKey{}
	}
	buf := make([]byte, len(embedding)*4)
	for i, v := range embedding {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	sum := sha256.Sum256(buf)
	var k entryKey
	copy(k[:], sum[:16])
	return k
}

// cosine computes cosine similarity. Both inputs must be L2-normalized;
// skip sqrt+divide to mirror scorer.go::topPNearest.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}
