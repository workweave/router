// Package usage tracks per-credential subscription rate-limit headroom observed
// from upstream response headers, and turns it into a routing cost signal.
//
// Both subscription backends report remaining quota on every response:
//   - Codex (chatgpt.com/backend-api/codex): x-codex-primary-* (rolling, ~5h)
//     and x-codex-secondary-* (weekly) — used-percent + window length.
//   - Claude (api.anthropic.com, OAuth): anthropic-ratelimit-unified-{5h,weekly}-*
//     — the same data `claude /usage` reads.
//
// These quotas are PERISHABLE: they reset every window, so unused headroom has
// zero salvage value. The marginal cost of a covered-model turn is therefore
// ~0 while the window has slack and only rises as the window approaches its cap
// — use-it-or-lose-it / bid-price control. CostFactor turns the observed
// utilization into a multiplier on a covered model's catalog cost: ~epsilon
// when slack, up to 1.0 (full price, no subsidy) as the window binds.
//
// Inner-ring + I/O-free: pure types, maps, a mutex, and an injected clock. No
// network, no DB, no goroutines (the composition root drives Sweep on a ticker).
package usage

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CredentialKey identifies a subscription credential without revealing it: an
// HMAC-SHA256 prefix of the token, keyed by a process-scoped salt. Safe to
// log/metric. This is a keyed identifier over a high-entropy token, NOT password
// storage — a slow KDF (bcrypt/argon2) is unnecessary because the token isn't
// brute-forceable; HMAC keeps the salt as the MAC key so a leaked key can't be
// reversed via a precomputed table.
type CredentialKey string

// KeyFor derives the CredentialKey for a subscription token under salt.
func KeyFor(salt, token []byte) CredentialKey {
	mac := hmac.New(sha256.New, salt)
	mac.Write(token)
	sum := mac.Sum(nil)
	return CredentialKey(hex.EncodeToString(sum[:8]))
}

// Window is one rate-limit window's observed state.
type Window struct {
	// UsedPercent is the fraction of the window's quota consumed, in [0,1].
	UsedPercent float64
	// WindowMinutes is the window length (0 if the upstream didn't report it).
	WindowMinutes int
}

func (w Window) present() bool { return w.WindowMinutes > 0 || w.UsedPercent > 0 }

// Snapshot is the most recent observation for one credential: a short rolling
// window (primary, ~5h) and a long window (secondary, weekly). Either may be
// zero if the upstream didn't report it.
type Snapshot struct {
	Primary    Window
	Secondary  Window
	ObservedAt time.Time
}

func (s Snapshot) hasData() bool { return s.Primary.present() || s.Secondary.present() }

// CostFactor maps observed utilization to a multiplier on a covered model's
// catalog cost: epsilon when the binding window has slack, rising to 1.0 as it
// approaches its cap. The tighter (more-used) of the two windows governs.
//
//	factor = epsilon + (1-epsilon) * u^gamma     (clamped to [epsilon, 1])
//
// gamma > 1 keeps the factor near epsilon until utilization is genuinely high,
// encoding the perishability bias (spend the quota you'd otherwise waste, back
// off only as the cap nears). epsilon > 0 so a covered model never reads as
// strictly free (which would dominate every quality tie). A snapshot with no
// usable data returns 1.0 — no subsidy until we've actually observed headroom.
func (s Snapshot) CostFactor(epsilon, gamma float64) float64 {
	if !s.hasData() {
		return 1.0
	}
	u := math.Max(s.Primary.UsedPercent, s.Secondary.UsedPercent)
	u = math.Min(1, math.Max(0, u))
	factor := epsilon + (1-epsilon)*math.Pow(u, gamma)
	return math.Min(1, math.Max(epsilon, factor))
}

// Observer stores the most recent Snapshot per credential. Concurrency-safe;
// each entry stays authoritative for the life of its binding quota window (see
// freshFor) — stale headroom must not pin the cost signal, but a reading must not
// age out faster than the window it describes.
type Observer struct {
	mu   sync.RWMutex
	salt []byte
	ttl  time.Duration
	now  func() time.Time
	data map[CredentialKey]Snapshot
}

// NewObserver builds an Observer. salt keys credentials; ttl is the MINIMUM
// freshness floor (for readings that carry no window length) — a reading that
// does report a window stays fresh for that window, not just ttl; now is the
// injected clock (real time in prod, fake in tests).
func NewObserver(salt []byte, ttl time.Duration, now func() time.Time) *Observer {
	if now == nil {
		now = time.Now
	}
	return &Observer{salt: salt, ttl: ttl, now: now, data: make(map[CredentialKey]Snapshot)}
}

// windowConstrainedFraction is the utilization at/above which a window is a real
// constraint whose full length must be waited out before its quota resets. Below
// it a window is effectively slack — its CostFactor contribution is already near
// epsilon — so it need not extend retention, and re-reading it as cold-start
// slack costs nothing.
const windowConstrainedFraction = 0.5

// freshFor reports how long a snapshot stays authoritative. Quota is perishable
// and refills only when its window resets, so a reading is meaningful until every
// window that is actually near cap would reset — NOT a flat ttl far shorter than
// any quota window (5h / weekly). Without this a near-cap reading ages out after
// the short ttl, Snapshot returns false, and the cold-start path re-applies the
// optimistic epsilon to a credential that is in fact still capped — routing back
// into it until a fresh response or 429 corrects it.
//
// The horizon is the LONGEST window among those at/above windowConstrainedFraction
// (not just the binding/most-utilized one): when the 5h primary binds CostFactor
// but the weekly window is also near cap, the entry must outlive the 5h window so
// it does not reset to optimistic while weekly quota is still exhausted. A slack
// window does not extend the horizon, so a 5h-capped + weekly-slack reading still
// expires at ~5h rather than being stranded at full price for a week. Floored at
// ttl so a reading carrying no constrained window still expires promptly; once
// every constrained window has elapsed the entry is evicted and the credential
// reads as never-observed again — correct, its quota has by then reset.
func (o *Observer) freshFor(s Snapshot) time.Duration {
	horizon := o.ttl
	for _, w := range [...]Window{s.Primary, s.Secondary} {
		if w.UsedPercent < windowConstrainedFraction {
			continue
		}
		if d := time.Duration(w.WindowMinutes) * time.Minute; d > horizon {
			horizon = d
		}
	}
	return horizon
}

// Key derives the CredentialKey for a token under this observer's salt.
func (o *Observer) Key(token []byte) CredentialKey { return KeyFor(o.salt, token) }

// Record stores a snapshot for a credential, stamped with the current time.
// A snapshot with no usable window is ignored (don't overwrite good data with
// an empty observation from a response that carried no rate-limit headers).
func (o *Observer) Record(key CredentialKey, snap Snapshot) {
	if !snap.hasData() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// Merge per-window with the prior (non-stale) observation: a single response
	// may report only one window, and replacing the whole snapshot would erase
	// the other window's last-known utilization — making CostFactor look slack
	// and over-discounting until TTL. A genuinely reset window reports used≈0
	// (still present), so it correctly overwrites; only an OMITTED window is
	// preserved from the prior snapshot.
	if prev, ok := o.data[key]; ok && o.now().Sub(prev.ObservedAt) <= o.freshFor(prev) {
		if !snap.Primary.present() {
			snap.Primary = prev.Primary
		}
		if !snap.Secondary.present() {
			snap.Secondary = prev.Secondary
		}
	}
	snap.ObservedAt = o.now()
	o.data[key] = snap
}

// Snapshot returns the most recent non-stale observation for a credential, or
// (zero, false) if none / expired. Expired entries are dropped lazily.
func (o *Observer) Snapshot(key CredentialKey) (Snapshot, bool) {
	o.mu.RLock()
	snap, ok := o.data[key]
	o.mu.RUnlock()
	if !ok {
		return Snapshot{}, false
	}
	if o.now().Sub(snap.ObservedAt) > o.freshFor(snap) {
		o.mu.Lock()
		if cur, still := o.data[key]; still && o.now().Sub(cur.ObservedAt) > o.freshFor(cur) {
			delete(o.data, key)
		}
		o.mu.Unlock()
		return Snapshot{}, false
	}
	return snap, true
}

// Sweep evicts all expired entries. The composition root calls this on a ticker
// to bound memory; the package spawns no goroutines of its own.
func (o *Observer) Sweep() {
	cutoff := o.now()
	o.mu.Lock()
	for k, s := range o.data {
		if cutoff.Sub(s.ObservedAt) > o.freshFor(s) {
			delete(o.data, k)
		}
	}
	o.mu.Unlock()
}

// ---- Header parsing --------------------------------------------------------

// ParseCodexHeaders extracts a Snapshot from the Codex backend's x-codex-*
// rate-limit headers (primary = rolling/~5h, secondary = weekly). Reports false
// if neither window is present. Used-percent headers are 0-100; we store [0,1].
func ParseCodexHeaders(h http.Header) (Snapshot, bool) {
	primary, pOK := parseCodexWindow(h, "primary", 5*60)
	secondary, sOK := parseCodexWindow(h, "secondary", 7*24*60)
	if !pOK && !sOK {
		return Snapshot{}, false
	}
	return Snapshot{Primary: primary, Secondary: secondary}, true
}

// parseCodexWindow reads one Codex window. defaultWindowMinutes is the known
// window length (primary ~5h, secondary weekly) used when the upstream omits the
// x-codex-*-window-minutes header — mirroring ParseAnthropicUnifiedHeaders, which
// hardcodes its window lengths. This guarantees every observed reading carries a
// window length, so freshFor never falls back to the short ttl floor for a real
// subscription: a near-cap Codex reading keeps suppressing the subsidy for the
// life of its window rather than aging out and re-applying the optimistic
// epsilon to a still-capped credential.
func parseCodexWindow(h http.Header, which string, defaultWindowMinutes int) (Window, bool) {
	used, ok := parsePercent(h.Get("x-codex-" + which + "-used-percent"))
	if !ok {
		return Window{}, false
	}
	w := Window{UsedPercent: used, WindowMinutes: defaultWindowMinutes}
	if m, ok := parseInt(h.Get("x-codex-" + which + "-window-minutes")); ok {
		w.WindowMinutes = m
	}
	return w, true
}

// ParseAnthropicUnifiedHeaders extracts a Snapshot from Anthropic's unified
// subscription rate-limit headers (the `claude /usage` data):
// anthropic-ratelimit-unified-5h-* (primary) and -weekly-* (secondary). Prefers
// an explicit *-utilization header; else derives used = 1 - remaining/limit.
// Reports false if neither window is present.
func ParseAnthropicUnifiedHeaders(h http.Header) (Snapshot, bool) {
	primary, pOK := parseAnthropicWindow(h, "5h", 5*60)
	secondary, sOK := parseAnthropicWindow(h, "weekly", 7*24*60)
	if !pOK && !sOK {
		return Snapshot{}, false
	}
	return Snapshot{Primary: primary, Secondary: secondary}, true
}

func parseAnthropicWindow(h http.Header, which string, windowMinutes int) (Window, bool) {
	prefix := "anthropic-ratelimit-unified-" + which + "-"
	if used, ok := parsePercent(h.Get(prefix + "utilization")); ok {
		return Window{UsedPercent: used, WindowMinutes: windowMinutes}, true
	}
	limit, lOK := parseFloat(h.Get(prefix + "limit"))
	remaining, rOK := parseFloat(h.Get(prefix + "remaining"))
	if !lOK || !rOK || limit <= 0 {
		return Window{}, false
	}
	used := math.Max(0, 1-remaining/limit)
	return Window{UsedPercent: used, WindowMinutes: windowMinutes}, true
}

// parsePercent parses a 0-100 percent header (Codex used-percent, Anthropic
// utilization) into a clamped [0,1] fraction. Always divides by 100 — both
// backends document these as 0-100 — rather than guessing the scale from the
// magnitude, which silently misreads the boundary (e.g. "1" = 1% must become
// 0.01, not 1.0). The remaining/limit path computes its fraction directly and
// does not call this.
func parsePercent(s string) (float64, bool) {
	v, ok := parseFloat(s)
	if !ok {
		return 0, false
	}
	v /= 100
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return v, true
}

func parseFloat(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func parseInt(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return v, true
}
