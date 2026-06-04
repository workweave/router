// Package sessionpin defines the inner-ring contract for session-sticky
// routing pins. Pure types + interface; the Postgres adapter lives in
// internal/postgres.
package sessionpin

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SessionKeyLen is the sha256-truncated session key length. 16 bytes makes
// collisions astronomically rare at per-(api-key, session) granularity.
const SessionKeyLen = 16

// DefaultRole is the only role emitted in Stage 1; column exists so the
// turn-type detector can land non-breakingly.
const DefaultRole = "default"

// Pin is one row in the session-pin table. Mirrors a routing decision
// so a hit can rehydrate router.Decision without re-running the scorer.
//
// Last*Tokens / LastTurnEndedAt record the previous turn's upstream usage
// and are written by Store.UpdateUsage after a turn completes. They are
// deliberately left untouched by Upsert so an at-start-of-turn refresh
// cannot clobber them with zeros.
//
// ConsecutiveUpstreamErrors counts consecutive turns ending in a
// non-retryable upstream error (4xx other than 408/429). The turn loop
// increments via Store.IncrementUpstreamErrors after a sticky-pinned
// turn fails and evicts the pin once the count hits the two-strike
// threshold; Store.ResetUpstreamErrors clears it on any successful
// turn. Upsert preserves it on a same-model refresh and resets it on
// a switch (different model).
type Pin struct {
	SessionKey                [SessionKeyLen]byte
	Role                      string
	InstallationID            uuid.UUID
	Provider                  string
	Model                     string
	Reason                    string
	TurnCount                 int
	PinnedUntil               time.Time
	FirstPinnedAt             time.Time
	LastSeenAt                time.Time
	LastInputTokens           int
	LastCachedReadTokens      int
	LastCachedWriteTokens     int
	LastOutputTokens          int
	LastTurnEndedAt           time.Time
	ConsecutiveUpstreamErrors int
	// LastServedModel is the model that actually served the previous turn,
	// written by Store.UpdateUsage (post-turn) and left untouched by Upsert.
	// A /force-model pin overwrites Model via Upsert but not this field, so
	// the next turn can compare it against the new target model to detect a
	// mid-session model switch (and strip Anthropic thinking-block signatures
	// the new model would otherwise reject with a 400).
	LastServedModel string
	// HasEverSwitched latches true the first time this session served a model
	// different from the prior LastServedModel. It stays set for the life of
	// the pin and is flipped atomically inside Store.UpdateUsage. The emit
	// path ORs it into ModelSwitched so the stale-signed thinking blocks an
	// earlier cross-model excursion left in the client transcript are stripped
	// on every subsequent turn, not just the single switch-back turn.
	HasEverSwitched bool
}

// Usage captures the previous turn's upstream token accounting.
type Usage struct {
	InputTokens       int
	CachedReadTokens  int
	CachedWriteTokens int
	OutputTokens      int
	EndedAt           time.Time
	// ServedModel is the model that served the turn this usage came from.
	ServedModel string
}

// Store is the I/O surface for session pins. Get returns (zero, false, nil)
// when no row exists. UpdateUsage, IncrementUpstreamErrors, and
// ResetUpstreamErrors are no-ops when the pin has been evicted or never
// existed.
//
// IncrementUpstreamErrors atomically increments the consecutive-error
// counter on the existing pin and returns the new count, so the turn
// loop can apply a two-strike eviction without a read-modify-write race
// across pods. Returns (0, nil) for a missing pin — the caller treats
// that as "pin already evicted" rather than an error.
type Store interface {
	Get(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) (Pin, bool, error)
	Upsert(ctx context.Context, p Pin) error
	UpdateUsage(ctx context.Context, sessionKey [SessionKeyLen]byte, role string, usage Usage) error
	IncrementUpstreamErrors(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) (int, error)
	ResetUpstreamErrors(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) error
	SweepExpired(ctx context.Context) error
}
