package sessionpin

import (
	"time"
)

// CacheLedger is the per-session map of recently served (provider, model)
// pairs and their last-turn usage, keyed by LedgerKey. It is maintained
// atomically in SQL on usage writeback (UpdateSessionPinUsage); Go only
// reads it to derive cache-warmth features at decision time. Entry count is
// bounded in practice by the deployed roster size and the pin row's TTL, so
// no LRU trim is enforced on write.
type CacheLedger map[string]LedgerEntry

// LedgerEntry records the last completed turn served by one
// (provider, model) pair within the session.
type LedgerEntry struct {
	LastTurnAt              time.Time `json:"last_turn_at"`
	LastInputTokens         int       `json:"last_input_tokens"`
	LastCacheReadTokens     int       `json:"last_cache_read_tokens"`
	LastCacheCreationTokens int       `json:"last_cache_creation_tokens"`
	LastOutputTokens        int       `json:"last_output_tokens"`
	ConsecutiveTurns        int       `json:"consecutive_turns"`
	// LastReconcileErrorTokens is the signed gap between the cache reuse a
	// warm ledger entry predicted (the entry's previous last_input_tokens)
	// and the cache_read tokens the provider actually reported, written on
	// the first revisit of an existing entry. Nil until a pair is revisited.
	// Persistently large magnitudes mark a provider whose cache is
	// best-effort; the value is a learnable reliability feature, not an
	// alarm.
	LastReconcileErrorTokens *int `json:"last_reconcile_error_tokens,omitempty"`
}

// LedgerKey is the CacheLedger map key for a (provider, model) pair.
func LedgerKey(provider, model string) string {
	return provider + "/" + model
}

// WarmPrefixTokens estimates the warm cached prefix a candidate
// (provider, model) still holds for this session: the entry's last input
// size while the provider's cache TTL has not lapsed, else 0. Prefixes grow
// monotonically within a CC session, so last input is a lower bound on the
// reusable prefix.
func (l CacheLedger) WarmPrefixTokens(provider, model string, now time.Time, ttl time.Duration) int {
	e, ok := l[LedgerKey(provider, model)]
	if !ok || ttl <= 0 || now.Sub(e.LastTurnAt) >= ttl {
		return 0
	}
	return e.LastInputTokens
}

// TTLRemainingFrac is the fraction of the provider cache TTL still
// remaining for a (provider, model) entry, in [0, 1]. 0 for unknown pairs,
// lapsed entries, or non-positive TTLs.
func (l CacheLedger) TTLRemainingFrac(provider, model string, now time.Time, ttl time.Duration) float64 {
	e, ok := l[LedgerKey(provider, model)]
	if !ok || ttl <= 0 {
		return 0
	}
	remaining := ttl - now.Sub(e.LastTurnAt)
	if remaining <= 0 {
		return 0
	}
	return float64(remaining) / float64(ttl)
}
