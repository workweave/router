package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/sqlc"

	"github.com/jackc/pgx/v5/pgtype"
)

// SessionPinRepo adapts sessionpin.Store to the SQLC-generated queries.
type SessionPinRepo struct {
	tx sqlc.DBTX
}

// NewSessionPinRepo wires the adapter over a pgx pool or transaction.
func NewSessionPinRepo(tx sqlc.DBTX) *SessionPinRepo {
	return &SessionPinRepo{tx: tx}
}

var _ sessionpin.Store = (*SessionPinRepo)(nil)

func (r *SessionPinRepo) Get(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) (sessionpin.Pin, bool, error) {
	q := sqlc.New(r.tx)
	row, err := q.GetSessionPin(ctx, sqlc.GetSessionPinParams{
		SessionKey: sessionKey[:],
		Role:       role,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sessionpin.Pin{}, false, nil
		}
		return sessionpin.Pin{}, false, err
	}
	return toSessionPin(row), true, nil
}

func (r *SessionPinRepo) Upsert(ctx context.Context, p sessionpin.Pin) error {
	q := sqlc.New(r.tx)
	return q.UpsertSessionPin(ctx, sqlc.UpsertSessionPinParams{
		SessionKey:     p.SessionKey[:],
		Role:           p.Role,
		InstallationID: p.InstallationID,
		PinnedProvider: p.Provider,
		PinnedModel:    p.Model,
		DecisionReason: p.Reason,
		TurnCount:      int32(p.TurnCount),
		PinnedUntil:    pgtype.Timestamp{Time: p.PinnedUntil.UTC(), Valid: true},
	})
}

// UpdateUsage records the previous turn's upstream token usage on the
// existing pin row. The UPDATE matches by (session_key, role); a
// missing pin (evicted, never created, or already swept) affects zero
// rows and surfaces as a successful no-op so callers off the request
// path don't have to special-case it. A zero-valued EndedAt is
// defensively stamped with time.Now so the column is always populated
// once a turn has produced usage.
func (r *SessionPinRepo) UpdateUsage(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string, usage sessionpin.Usage) error {
	endedAt := usage.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	q := sqlc.New(r.tx)
	return q.UpdateSessionPinUsage(ctx, sqlc.UpdateSessionPinUsageParams{
		SessionKey:            sessionKey[:],
		Role:                  role,
		LastInputTokens:       int32(usage.InputTokens),
		LastCachedReadTokens:  int32(usage.CachedReadTokens),
		LastCachedWriteTokens: int32(usage.CachedWriteTokens),
		LastOutputTokens:      int32(usage.OutputTokens),
		LastTurnEndedAt:       pgtype.Timestamptz{Time: endedAt.UTC(), Valid: true},
		LastServedModel:       usage.ServedModel,
	})
}

// IncrementUpstreamErrors atomically bumps the consecutive-error
// counter and returns the new value. A missing pin (already evicted
// by force-model or loop-break, or never created) surfaces as
// (0, nil) so the turn loop's two-strike check treats it as a no-op
// — the relevant signal is gone and there's no row left to evict.
func (r *SessionPinRepo) IncrementUpstreamErrors(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) (int, error) {
	q := sqlc.New(r.tx)
	count, err := q.IncrementSessionPinUpstreamErrors(ctx, sqlc.IncrementSessionPinUpstreamErrorsParams{
		SessionKey: sessionKey[:],
		Role:       role,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return int(count), nil
}

// ResetUpstreamErrors clears the consecutive-error counter after a
// successful turn. Missing pin = successful no-op (mirrors
// UpdateUsage's semantics).
func (r *SessionPinRepo) ResetUpstreamErrors(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string) error {
	q := sqlc.New(r.tx)
	return q.ResetSessionPinUpstreamErrors(ctx, sqlc.ResetSessionPinUpstreamErrorsParams{
		SessionKey: sessionKey[:],
		Role:       role,
	})
}

func (r *SessionPinRepo) SweepExpired(ctx context.Context) error {
	q := sqlc.New(r.tx)
	return q.SweepExpiredSessionPins(ctx)
}

func toSessionPin(row sqlc.RouterSessionPin) sessionpin.Pin {
	pin := sessionpin.Pin{
		Role:                  row.Role,
		InstallationID:        row.InstallationID,
		Provider:              row.PinnedProvider,
		Model:                 row.PinnedModel,
		Reason:                row.DecisionReason,
		TurnCount:             int(row.TurnCount),
		PinnedUntil:           timestampOrZero(row.PinnedUntil),
		FirstPinnedAt:         timestampOrZero(row.FirstPinnedAt),
		LastSeenAt:            timestampOrZero(row.LastSeenAt),
		LastInputTokens:       int(row.LastInputTokens),
		LastCachedReadTokens:  int(row.LastCachedReadTokens),
		LastCachedWriteTokens: int(row.LastCachedWriteTokens),
		LastOutputTokens:      int(row.LastOutputTokens),
		LastTurnEndedAt:       timestamptzOrZero(row.LastTurnEndedAt),
		LastServedModel:       row.LastServedModel,
		HasEverSwitched:       row.HasEverSwitched,
	}
	// Bounded copy guards against a corrupt row panicking the request handler.
	copy(pin.SessionKey[:], row.SessionKey)
	return pin
}

// timestamptzOrZero mirrors timestampOrZero for TIMESTAMPTZ columns;
// NULL on the wire is surfaced as the time.Time zero value so callers
// can branch on IsZero() rather than threading a pointer through.
func timestamptzOrZero(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}
