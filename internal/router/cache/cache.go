// Package cache short-circuits near-duplicate non-streaming requests by cosine
// similarity on prompt embedding, isolated per (installation, inbound-format).
// Entries expire lazily on Lookup once past the configured TTL.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"workweave/router/internal/observability"
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
	// BucketSize caps each per-(installation, format, clusterID, clusterVersion,
	// knobsHash) LRU.
	BucketSize int
	// MaxBucketsPerInstallation caps buckets per installation. Bucket identity
	// includes attacker-influenceable inputs (cluster version, knobs hash), so
	// without this cap one tenant could vary x-weave-routing-* headers to evict
	// other tenants' buckets from a shared global LRU.
	MaxBucketsPerInstallation int
	// MaxInstallations caps tracked installations. Defense in depth since
	// installations come from bearer tokens, not request headers.
	MaxInstallations int
	TTL              time.Duration
	MaxBodyBytes     int
}

// DefaultConfig returns conservative defaults tuned for staging.
func DefaultConfig() Config {
	return Config{
		DefaultThreshold:          0.95,
		BucketSize:                1024,
		MaxBucketsPerInstallation: 512,
		MaxInstallations:          1024,
		TTL:                       1 * time.Hour,
		MaxBodyBytes:              1 << 20,
	}
}

// Cache is the in-memory semantic cache. Concurrent-safe.
//
// TTL is enforced lazily via `storedAt` on Lookup, not a background goroutine:
// expirable.LRU's v2.0.7 cleanup goroutine has no public shutdown, so using it
// for inner buckets would leak one goroutine per bucket evicted past
// MaxBucketsPerInstallation.
type Cache struct {
	cfg           Config
	now           func() time.Time
	installations *lru.Cache[string, *installationCache]
	mu            sync.Mutex
}

type installationCache struct {
	buckets *lru.Cache[bucketKey, *lru.Cache[entryKey, *entry]]
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
	if cfg.MaxBucketsPerInstallation <= 0 {
		cfg.MaxBucketsPerInstallation = def.MaxBucketsPerInstallation
	}
	if cfg.MaxInstallations <= 0 {
		cfg.MaxInstallations = def.MaxInstallations
	}
	if cfg.TTL <= 0 {
		cfg.TTL = def.TTL
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = def.MaxBodyBytes
	}
	installations, err := lru.New[string, *installationCache](cfg.MaxInstallations)
	if err != nil {
		// lru.New only errors on size <= 0, which we just guarded against.
		panic(err)
	}
	return &Cache{
		cfg:           cfg,
		now:           time.Now,
		installations: installations,
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
	format         Format
	clusterID      int
	clusterVersion string
	knobsHash      uint64
}

// entryKey is a 16-byte sha256-truncated embedding digest, so we don't
// store the 3KB embedding twice as the LRU key.
type entryKey [16]byte

type entry struct {
	embedding []float32
	response  CachedResponse
	storedAt  time.Time
}

// thresholdFor returns the cosine threshold for clusterID, falling
// through to the default.
func (c *Cache) thresholdFor(clusterID int) float32 {
	if t, ok := c.cfg.PerClusterThreshold[clusterID]; ok {
		return t
	}
	return c.cfg.DefaultThreshold
}

// Lookup walks buckets for (installation, format, clusterIDs) and returns the
// first entry whose cosine clears the threshold. embedding must be L2-normalized.
func (c *Cache) Lookup(installationID string, format Format, embedding []float32, clusterIDs []int, clusterVersion string, knobsHash uint64) (CachedResponse, bool) {
	if c == nil || len(embedding) == 0 || len(clusterIDs) == 0 {
		return CachedResponse{}, false
	}
	now := c.now()
	for _, cid := range clusterIDs {
		bucket := c.bucket(installationID, format, cid, clusterVersion, knobsHash, false)
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
			if now.Sub(e.storedAt) > c.cfg.TTL {
				bucket.Remove(k)
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
func (c *Cache) Store(installationID string, format Format, embedding []float32, clusterID int, resp CachedResponse, clusterVersion string, knobsHash uint64) {
	if c == nil || len(embedding) == 0 {
		return
	}
	if len(resp.Body) > c.cfg.MaxBodyBytes {
		return
	}
	bucket := c.bucket(installationID, format, clusterID, clusterVersion, knobsHash, true)
	if bucket == nil {
		return
	}
	// Deep-copy: caller's slice may be mutated/recycled.
	embedCopy := make([]float32, len(embedding))
	copy(embedCopy, embedding)
	bucket.Add(entryKeyFor(embedCopy), &entry{
		embedding: embedCopy,
		response:  resp,
		storedAt:  c.now(),
	})
}

// bucket returns the LRU for a key. create=false returns nil if missing
// (lookup path); create=true allocates lazily, capped per-installation.
func (c *Cache) bucket(installationID string, format Format, clusterID int, clusterVersion string, knobsHash uint64, create bool) *lru.Cache[entryKey, *entry] {
	key := bucketKey{
		format:         format,
		clusterID:      clusterID,
		clusterVersion: clusterVersion,
		knobsHash:      knobsHash,
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ic, ok := c.installations.Get(installationID)
	if !ok {
		if !create {
			return nil
		}
		buckets, err := lru.New[bucketKey, *lru.Cache[entryKey, *entry]](c.cfg.MaxBucketsPerInstallation)
		if err != nil {
			observability.Get().Error("cache: failed to allocate installation bucket LRU; treating as cache miss", "err", err)
			return nil
		}
		ic = &installationCache{buckets: buckets}
		c.installations.Add(installationID, ic)
	}
	b, ok := ic.buckets.Get(key)
	if !ok {
		if !create {
			return nil
		}
		b, err := lru.New[entryKey, *entry](c.cfg.BucketSize)
		if err != nil {
			observability.Get().Error("cache: failed to allocate entry LRU; treating as cache miss", "err", err)
			return nil
		}
		ic.buckets.Add(key, b)
		return b
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
