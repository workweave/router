package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
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

func (r *SessionPinRepo) Get(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string, installationID uuid.UUID) (sessionpin.Pin, bool, error) {
	q := sqlc.New(r.tx)
	row, err := q.GetSessionPin(ctx, sqlc.GetSessionPinParams{
		SessionKey:     sessionKey[:],
		Role:           role,
		InstallationID: installationID,
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
		PairedProvider: p.PairedProvider,
		PairedModel:    p.PairedModel,
		DecisionReason: p.Reason,
		TurnCount:      int32(p.TurnCount),
		PinnedUntil:    pgtype.Timestamp{Time: p.PinnedUntil.UTC(), Valid: true},
	})
}

// UpdateUsage records the previous turn's usage on the pin row. A missing
// pin (evicted/swept/never created) or ownership mismatch is a no-op, not
// an error. A zero EndedAt is stamped with time.Now so the column is always
// populated.
func (r *SessionPinRepo) UpdateUsage(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string, installationID uuid.UUID, usage sessionpin.Usage) error {
	endedAt := usage.EndedAt
	if endedAt.IsZero() {
		endedAt = time.Now()
	}
	q := sqlc.New(r.tx)
	return q.UpdateSessionPinUsage(ctx, sqlc.UpdateSessionPinUsageParams{
		SessionKey:            sessionKey[:],
		Role:                  role,
		InstallationID:        installationID,
		LastInputTokens:       int32(usage.InputTokens),
		LastCachedReadTokens:  int32(usage.CachedReadTokens),
		LastCachedWriteTokens: int32(usage.CachedWriteTokens),
		LastOutputTokens:      int32(usage.OutputTokens),
		LastTurnEndedAt:       pgtype.Timestamptz{Time: endedAt.UTC(), Valid: true},
		LastServedModel:       usage.ServedModel,
		PriorServedModel:      usage.PriorServedModel,
		SessionEverSwitched:   usage.SessionEverSwitched,
	})
}

// IncrementUpstreamErrors atomically bumps the consecutive-error counter.
// A missing pin (already evicted, never created, or ownership mismatch)
// returns (0, nil): the two-strike check treats it as a no-op since there's
// no row left to evict.
func (r *SessionPinRepo) IncrementUpstreamErrors(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string, installationID uuid.UUID) (int, error) {
	q := sqlc.New(r.tx)
	count, err := q.IncrementSessionPinUpstreamErrors(ctx, sqlc.IncrementSessionPinUpstreamErrorsParams{
		SessionKey:     sessionKey[:],
		Role:           role,
		InstallationID: installationID,
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
// successful turn. Missing pin / ownership mismatch is a no-op, same as
// UpdateUsage.
func (r *SessionPinRepo) ResetUpstreamErrors(ctx context.Context, sessionKey [sessionpin.SessionKeyLen]byte, role string, installationID uuid.UUID) error {
	q := sqlc.New(r.tx)
	return q.ResetSessionPinUpstreamErrors(ctx, sqlc.ResetSessionPinUpstreamErrorsParams{
		SessionKey:     sessionKey[:],
		Role:           role,
		InstallationID: installationID,
	})
}

func (r *SessionPinRepo) SweepExpired(ctx context.Context) error {
	q := sqlc.New(r.tx)
	return q.SweepExpiredSessionPins(ctx)
}

func toSessionPin(row sqlc.RouterSessionPin) sessionpin.Pin {
	pin := sessionpin.Pin{
		Role:                      row.Role,
		InstallationID:            row.InstallationID,
		Provider:                  row.PinnedProvider,
		Model:                     row.PinnedModel,
		PairedProvider:            row.PairedProvider,
		PairedModel:               row.PairedModel,
		Reason:                    row.DecisionReason,
		TurnCount:                 int(row.TurnCount),
		PinnedUntil:               timestampOrZero(row.PinnedUntil),
		FirstPinnedAt:             timestampOrZero(row.FirstPinnedAt),
		LastSeenAt:                timestampOrZero(row.LastSeenAt),
		LastInputTokens:           int(row.LastInputTokens),
		LastCachedReadTokens:      int(row.LastCachedReadTokens),
		LastCachedWriteTokens:     int(row.LastCachedWriteTokens),
		LastOutputTokens:          int(row.LastOutputTokens),
		LastTurnEndedAt:           timestamptzOrZero(row.LastTurnEndedAt),
		LastServedModel:           row.LastServedModel,
		HasEverSwitched:           row.HasEverSwitched,
		ConsecutiveUpstreamErrors: int(row.ConsecutiveUpstreamErrors),
	}
	// Bounded copy guards against a corrupt row panicking the request handler.
	copy(pin.SessionKey[:], row.SessionKey)
	return pin
}

// timestamptzOrZero mirrors timestampOrZero for TIMESTAMPTZ columns:
// NULL becomes the zero value instead of a pointer.
func timestamptzOrZero(t pgtype.Timestamptz) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time
}
