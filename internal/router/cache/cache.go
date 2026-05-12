// Package cache implements a semantic response cache for the router.
//
// Short-circuits near-duplicate requests by cosine similarity on the
// cluster scorer's prompt embedding; on hit, captured wire-format bytes
// are replayed without invoking the upstream provider.
//
// Scope (v1):
//   - Non-streaming only. Proxy bypasses cache when stream=true.
//   - Per-(installation, inbound-format) isolation. Bucket key includes
//     inbound format because captured bytes are post-translation, so a
//     cached Anthropic response must never replay for an OpenAI client.
//   - Cluster-bucketed brute-force lookup. Each (installation, format,
//     clusterID) owns an LRU; lookup scans the routing decision's top-p
//     clusters. At ~10 clusters × 1k entries × 768-d, fits in <5ms;
//     HNSW is unnecessary.
//
// Out of scope: streaming, distributed (Redis), cross-installation
// sharing, event-driven invalidation. TTL is the only eviction.
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

// CachedResponse captures the upstream response in the inbound wire
// format. Stores post-translation bytes so replay is indistinguishable
// from the client's perspective.
type CachedResponse struct {
	StatusCode int
	Headers    http.Header
	// Body is bounded by MaxBodyBytes; oversized entries are dropped.
	Body []byte
}

// Config carries the runtime knobs.
type Config struct {
	PerClusterThreshold map[int]float32
	DefaultThreshold    float32
	// BucketSize caps per-(installation, format, clusterID) LRU size so
	// one chatty installation can't evict another's entries.
	BucketSize int
	TTL        time.Duration
	// MaxBodyBytes drops responses larger than this without caching
	// (Anthropic max_tokens is 200k ≈ 1MB).
	MaxBodyBytes int
}

// DefaultConfig returns conservative defaults tuned for staging.
func DefaultConfig() Config {
	return Config{
		DefaultThreshold: 0.95,
		BucketSize:       1024,
		TTL:              1 * time.Hour,
		MaxBodyBytes:     1 << 20, // 1 MiB
	}
}

// Cache is the in-memory semantic cache. Concurrent-safe.
type Cache struct {
	cfg     Config
	buckets map[bucketKey]*expirable.LRU[entryKey, *entry]
	mu      sync.Mutex // guards `buckets`; LRU itself is thread-safe
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
// returns the first entry whose cosine clears the cluster's threshold.
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
		// LRU.Keys() returns MRU-first, biasing toward the most-recently
		// stored similar entry.
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
// decision's top-p clusters; Lookup scans every top-p cluster so any
// one suffices. Oversized bodies are silently dropped.
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
	// Deep-copy: caller's slice is owned by router.Decision.Metadata
	// and may be mutated/recycled.
	embedCopy := make([]float32, len(embedding))
	copy(embedCopy, embedding)
	bucket.Add(entryKeyFor(embedCopy), &entry{
		embedding: embedCopy,
		response:  resp,
	})
}

// bucket returns the LRU for a key. create=false returns nil for
// missing buckets (lookup path); create=true allocates lazily.
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

// entryKeyFor hashes embedding bytes so identical embeddings produce
// the same key and re-stores update in-place rather than churning the LRU.
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

// cosine computes cosine similarity. Both inputs must be L2-normalized
// (cluster scorer guarantees this); skip sqrt+divide to mirror
// scorer.go::topPNearest.
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
