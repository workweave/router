// Package cache implements a semantic response cache for the router.
//
// The cache short-circuits identical and near-duplicate requests by
// keying on the cluster scorer's prompt embedding (cosine similarity
// against previously-stored entries). On a hit, the captured wire-
// format response bytes are replayed to the client without invoking
// the upstream provider.
//
// Scope (v1):
//
//   - Non-streaming requests only. The proxy bypasses the cache when
//     the inbound body has stream=true; streaming buffer-on-miss is a
//     follow-up. RouterArena's eval traffic is non-streaming, so the
//     v1 covers the benchmark surface.
//
//   - Per-(installation, inbound-format) isolation. Two tenants asking
//     the same question both pay first-call cost; a cached Anthropic
//     response is never replayed for an OpenAI client. Inbound format
//     is part of the bucket key because the captured bytes are
//     post-translation (Anthropic SSE for Anthropic clients,
//     OpenAI JSON for OpenAI clients).
//
//   - Cluster-bucketed brute-force lookup. Each (installationID,
//     inboundFormat, clusterID) tuple owns its own LRU. On lookup, the
//     cache scans the buckets for the routing decision's top-p clusters
//     and returns the first entry whose cosine ≥ the per-cluster
//     threshold. With 10 clusters and 1k entries per bucket, top-p=4
//     × 1k × 768-d cosine ops fits in <5ms; HNSW is unnecessary at
//     this scale.
//
// Out of scope: streaming, distributed (Redis), cross-installation
// sharing, event-driven invalidation. TTL is the only eviction
// mechanism.
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
// format so a cache hit can replay it byte-for-byte. We intentionally
// store the post-translation bytes (e.g. an Anthropic SSE response
// produced by translate.AnthropicSSETranslator when the upstream is
// OpenAI) rather than the upstream's native bytes — the goal is
// indistinguishable replay from the client's perspective.
type CachedResponse struct {
	// StatusCode is the HTTP status the proxy wrote (or 200 if
	// implicit).
	StatusCode int
	// Headers is a snapshot of the response headers at end-of-write.
	// Caller-set router headers (x-router-*) and upstream headers
	// (request-id, content-type, etc.) are both preserved; the cache
	// consumer can scrub or override per-hit.
	Headers http.Header
	// Body is the full response body (post-translation if
	// cross-format). Bounded by the cache's MaxBodyBytes; entries
	// exceeding the cap are dropped (not stored) so callers should
	// budget for a short tail of uncached large responses.
	Body []byte
}

// Config carries the runtime knobs.
type Config struct {
	// PerClusterThreshold overrides DefaultThreshold per cluster.
	// Empty/nil means "always use DefaultThreshold".
	PerClusterThreshold map[int]float32
	// DefaultThreshold is the cosine floor applied to every cluster
	// without an override. Conservative default 0.95.
	DefaultThreshold float32
	// BucketSize caps per-(installation, inboundFormat, clusterID)
	// LRU size. Bounded so a single chatty installation can't evict
	// another's entries.
	BucketSize int
	// TTL is the LRU's per-entry time-to-live.
	TTL time.Duration
	// MaxBodyBytes drops responses larger than this without caching.
	// Prevents an unbounded body from blowing memory on a single
	// outlier (Anthropic max_tokens is 200k tokens ≈ 1MB).
	MaxBodyBytes int
}

// DefaultConfig returns conservative defaults. Tuned for a single
// staging deployment; production may want larger BucketSize and a
// shorter TTL.
func DefaultConfig() Config {
	return Config{
		DefaultThreshold: 0.95,
		BucketSize:       1024,
		TTL:              1 * time.Hour,
		MaxBodyBytes:     1 << 20, // 1 MiB
	}
}

// Cache is the in-memory semantic cache. Safe for concurrent use; the
// outer map of buckets is mutex-guarded, and each per-bucket
// expirable.LRU is itself thread-safe.
type Cache struct {
	cfg     Config
	buckets map[bucketKey]*expirable.LRU[entryKey, *entry]
	mu      sync.Mutex // guards `buckets` map (LRU itself is thread-safe)
}

// New constructs a Cache. nil Config fields fall through to
// DefaultConfig().
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

// Format names the inbound wire format the cached response is encoded
// in. Lookups must match against the same Format the entry was stored
// under so an Anthropic client never receives an OpenAI-shaped reply.
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

// entryKey is a 16-byte digest of the embedding, derived from sha256
// truncation. It identifies an entry inside its bucket without storing
// the full 3KB embedding twice (the entry already carries it).
type entryKey [16]byte

type entry struct {
	embedding []float32
	response  CachedResponse
}

// thresholdFor returns the cosine threshold for a given cluster, with
// fall-through to the default.
func (c *Cache) thresholdFor(clusterID int) float32 {
	if t, ok := c.cfg.PerClusterThreshold[clusterID]; ok {
		return t
	}
	return c.cfg.DefaultThreshold
}

// Lookup walks the buckets for the given (installation, format,
// clusterIDs) tuples and returns the first entry whose cosine
// similarity against `embedding` clears the cluster's threshold.
//
// Embedding must be L2-normalized (matching the cluster scorer's
// output); cosine reduces to a dot product. clusterIDs is the
// scorer's top-p list. Order of clusterIDs is irrelevant — every
// listed bucket is scanned.
//
// The (CachedResponse, false) case never occurs; the bool is sized
// around the typical Go cache idiom of "value, hit".
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
		// Iterate the LRU's keys; on each hit-candidate compute
		// cosine. expirable.LRU.Keys() returns MRU-first which biases
		// us toward returning the most-recently-stored similar entry —
		// useful when the same prompt has been answered repeatedly.
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

// Store persists a response under (installationID, format, clusterID).
// clusterID should be one of the routing decision's top-p clusters
// (the scorer sorts them ascending; picking the smallest keeps storage
// deterministic). Lookup scans every top-p cluster, so any one is
// sufficient.
//
// Bodies exceeding cfg.MaxBodyBytes are silently dropped (no error,
// no panic) so a single outlier doesn't blow memory.
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
	// Deep-copy the embedding: the caller's slice is owned by
	// router.Decision.Metadata and may be mutated/recycled.
	embedCopy := make([]float32, len(embedding))
	copy(embedCopy, embedding)
	bucket.Add(entryKeyFor(embedCopy), &entry{
		embedding: embedCopy,
		response:  resp,
	})
}

// bucket returns the LRU for a given key. When create is false and the
// bucket doesn't exist, returns nil (lookup path: we skip the empty
// bucket). When create is true, the bucket is lazily allocated.
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

// entryKeyFor derives a deterministic key from an embedding by
// hashing its raw bytes. Two identical embeddings produce the same
// key, so re-storing the same prompt updates the existing entry
// in-place rather than churning the LRU.
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

// cosine computes the cosine similarity between two L2-normalized
// vectors. Both inputs must already be normalized (the cluster scorer
// guarantees this for its embeddings); skipping the sqrt + divide
// here mirrors scorer.go::topPNearest's reasoning.
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
