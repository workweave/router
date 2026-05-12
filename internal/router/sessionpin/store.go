// Package sessionpin defines the inner-ring contract for session-sticky
// routing pins. Pure types + interface; adapters live in
// internal/postgres so the proxy service can be tested without a DB.
//
// Stage 1 ships the schema as role-keyed but always emits role="default";
// role-conditioned pinning waits on the turn-type detector.
package sessionpin

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SessionKeyLen is the sha256-truncated session key length. 16 bytes
// makes collisions astronomically rare at per-(api-key, session)
// granularity and mirrors routing_observations.prompt_prefix_hash.
const SessionKeyLen = 16

// DefaultRole is the only role emitted in Stage 1; column exists so the
// turn-type detector can land non-breakingly.
const DefaultRole = "default"

// Pin is one row in the session-pin table. Mirrors a routing decision
// so a hit can rehydrate router.Decision without re-running the scorer.
type Pin struct {
	SessionKey     [SessionKeyLen]byte
	Role           string
	InstallationID uuid.UUID
	Provider       string
	Model          string
	Reason         string
	TurnCount      int
	PinnedUntil    time.Time
	FirstPinnedAt  time.Time
	LastSeenAt     time.Time
}

// Store is the I/O surface for session pins. Get returns (zero, false,
// nil) when no row exists (no error).
type Store interface {
	Get(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) (Pin, bool, error)
	Upsert(ctx context.Context, p Pin) error
	SweepExpired(ctx context.Context) error
}
