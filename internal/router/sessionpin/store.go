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
type Pin struct {
	SessionKey            [SessionKeyLen]byte
	Role                  string
	InstallationID        uuid.UUID
	Provider              string
	Model                 string
	Reason                string
	TurnCount             int
	PinnedUntil           time.Time
	FirstPinnedAt         time.Time
	LastSeenAt            time.Time
	LastInputTokens       int
	LastCachedReadTokens  int
	LastCachedWriteTokens int
	LastOutputTokens      int
	LastTurnEndedAt       time.Time
}

// Usage captures the previous turn's upstream token accounting.
type Usage struct {
	InputTokens       int
	CachedReadTokens  int
	CachedWriteTokens int
	OutputTokens      int
	EndedAt           time.Time
}

// Store is the I/O surface for session pins. Get returns (zero, false, nil)
// when no row exists. UpdateUsage is a no-op when the pin has been evicted
// or never existed.
type Store interface {
	Get(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) (Pin, bool, error)
	Upsert(ctx context.Context, p Pin) error
	UpdateUsage(ctx context.Context, sessionKey [SessionKeyLen]byte, role string, usage Usage) error
	SweepExpired(ctx context.Context) error
}
