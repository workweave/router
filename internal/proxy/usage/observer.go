// Package usage tracks the most recently observed Anthropic unified
// rate-limit utilization (the same data the `claude /usage` CLI reads
// off the `anthropic-ratelimit-unified-{5h,weekly}-*` response headers).
//
// The router uses this signal to decide whether to bypass cluster
// routing on a per-request basis: while the caller has headroom on
// both windows, requests pass straight through to Anthropic with the
// requested model; once either window crosses the configured threshold
// the cluster scorer takes over and may substitute a different model.
//
// The observer is pure in-memory state with no persistence. A short
// TTL on entries means a torn-down Anthropic key or a long idle
// period falls back to "cold start = bypass" rather than pinning the
// gate open with a stale near-100% reading.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Observation captures the most recent unified rate-limit readings
// for a single Anthropic credential. A negative value means the header
// was not present on the response (treated as "no signal" by the gate).
type Observation struct {
	FiveHourUtil float64
	WeeklyUtil   float64
	ObservedAt   time.Time
}

// Header names Anthropic returns on every Messages response. Kept as
// constants so callers (the Anthropic provider adapter) don't carry
// the wire spelling themselves.
const (
	HeaderFiveHourUtil = "anthropic-ratelimit-unified-5h-utilization"
	HeaderWeeklyUtil   = "anthropic-ratelimit-unified-weekly-utilization"
)

// ParseObservation extracts a single Observation from response headers.
// Missing or malformed values produce a -1 in that field. ObservedAt
// is set by the caller (Record); leave it zero here.
func ParseObservation(h http.Header) Observation {
	return Observation{
		FiveHourUtil: parseUtil(h.Get(HeaderFiveHourUtil)),
		WeeklyUtil:   parseUtil(h.Get(HeaderWeeklyUtil)),
	}
}

func parseUtil(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v
}

// HasSignal reports whether the observation carries any usable
// utilization value (at least one window parsed cleanly).
func (o Observation) HasSignal() bool {
	return o.FiveHourUtil >= 0 || o.WeeklyUtil >= 0
}

// CredentialKey derives a stable, log-safe key for an Anthropic
// credential. Returns "" for empty input so callers can short-circuit
// when no credential is present rather than recording under a single
// shared empty bucket.
func CredentialKey(apiKey []byte) string {
	if len(apiKey) == 0 {
		return ""
	}
	sum := sha256.Sum256(apiKey)
	return hex.EncodeToString(sum[:16])
}

// Observer is a concurrent, TTL-bounded cache of the most recent
// Observation per credential key. The zero value is not usable; use
// NewObserver. Threshold and TTL are fixed at construction.
type Observer struct {
	mu        sync.RWMutex
	entries   map[string]Observation
	threshold float64
	ttl       time.Duration
	now       func() time.Time
	enabled   bool
}

// NewObserver builds an Observer.
//
//   - threshold: utilization (0..1) at which routing engages. A request
//     keeps bypassing routing while both windows are strictly below it.
//   - ttl: observations older than this are treated as no-signal.
//   - enabled: when false the observer still records (so /admin tools
//     and logs can see live numbers) but ShouldBypassRouting always
//     returns true, preserving the legacy always-passthrough behavior
//     while we soak the feature in staging.
func NewObserver(threshold float64, ttl time.Duration, enabled bool) *Observer {
	return &Observer{
		entries:   make(map[string]Observation),
		threshold: threshold,
		ttl:       ttl,
		now:       time.Now,
		enabled:   enabled,
	}
}

// RecordObservation stores obs under key, stamping ObservedAt to the
// current clock. Skips entries without any usable signal so a single
// bad response can't evict a known-good prior reading.
func (o *Observer) RecordObservation(key string, obs Observation) {
	if o == nil || key == "" || !obs.HasSignal() {
		return
	}
	obs.ObservedAt = o.now()
	o.mu.Lock()
	o.entries[key] = obs
	o.mu.Unlock()
}

// Record adapts the provider-side UsageRecorder interface
// (key + raw http.Header) so the Observer can be plugged into the
// Anthropic adapter without that adapter importing this package's
// Observation type.
func (o *Observer) Record(key string, h http.Header) {
	if o == nil || h == nil {
		return
	}
	o.RecordObservation(key, ParseObservation(h))
}

// Latest returns the current Observation for key and whether one is
// present and still fresh (within TTL). Exposed for telemetry / tests.
func (o *Observer) Latest(key string) (Observation, bool) {
	if o == nil || key == "" {
		return Observation{}, false
	}
	o.mu.RLock()
	obs, ok := o.entries[key]
	o.mu.RUnlock()
	if !ok {
		return Observation{}, false
	}
	if o.now().Sub(obs.ObservedAt) > o.ttl {
		return obs, false
	}
	return obs, true
}

// ShouldBypassRouting reports whether the request for the given
// credential should skip cluster routing and pass straight through
// to Anthropic. The contract is:
//
//   - feature disabled: always true (preserves legacy behavior).
//   - empty key (no Anthropic credential resolvable): true; the caller
//     handles credential-less requests via the existing scorer path.
//   - no fresh observation: true (cold start; first request goes
//     through so we can learn the utilization headers from its response).
//   - fresh observation, both windows below threshold: true.
//   - fresh observation, either window at-or-above threshold: false
//     (engage cluster routing).
//
// A window whose header was missing on the last response is treated
// as below threshold for that window — we never engage routing on a
// missing signal.
func (o *Observer) ShouldBypassRouting(key string) bool {
	if o == nil || !o.enabled {
		return true
	}
	if key == "" {
		return true
	}
	obs, fresh := o.Latest(key)
	if !fresh {
		return true
	}
	if obs.FiveHourUtil >= o.threshold {
		return false
	}
	if obs.WeeklyUtil >= o.threshold {
		return false
	}
	return true
}

// Enabled reports the configured feature-flag state.
func (o *Observer) Enabled() bool {
	if o == nil {
		return false
	}
	return o.enabled
}

// Threshold reports the configured engagement threshold.
func (o *Observer) Threshold() float64 {
	if o == nil {
		return 0
	}
	return o.threshold
}
