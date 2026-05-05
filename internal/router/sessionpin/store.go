// Package sessionpin defines the inner-ring contract for session-sticky
// routing pins. Implementations live in adapter packages
// (internal/postgres/session_pin_repo.go); this package is pure types
// and an interface so the proxy service can be tested without a DB.
//
// Stage 1 ships the schema as role-keyed but always emits role="default";
// role-conditioned pinning waits on the §3.3 turn-type detector.
package sessionpin

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// SessionKeyLen is the byte length of a derived session key. 16 bytes
// (sha256-truncated) is enough to make collisions astronomically rare
// at the per-(api-key, session) granularity we key on, and fits the
// same shape as the planned routing_observations.prompt_prefix_hash.
const SessionKeyLen = 16

// DefaultRole is the only role emitted in Stage 1. Schema carries the
// column so the §3.3 turn-type detector can land non-breakingly.
const DefaultRole = "default"

// Pin is one row in the session-pin table. SessionKey is the 16-byte
// derived key (see internal/proxy/session_key.go); the rest mirrors the
// existing routing decision so a hit can rehydrate router.Decision
// without re-running the cluster scorer.
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

// Store is the I/O surface for session pins. The proxy service depends
// on this interface; adapters implement it. Get returns (zero, false,
// nil) when no row exists (no error).
type Store interface {
	Get(ctx context.Context, sessionKey [SessionKeyLen]byte, role string) (Pin, bool, error)
	Upsert(ctx context.Context, p Pin) error
	SweepExpired(ctx context.Context) error
}
