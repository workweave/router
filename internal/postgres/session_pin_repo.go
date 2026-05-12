package postgres

import (
	"context"
	"database/sql"
	"errors"

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

func (r *SessionPinRepo) SweepExpired(ctx context.Context) error {
	q := sqlc.New(r.tx)
	return q.SweepExpiredSessionPins(ctx)
}

func toSessionPin(row sqlc.RouterSessionPin) sessionpin.Pin {
	pin := sessionpin.Pin{
		Role:           row.Role,
		InstallationID: row.InstallationID,
		Provider:       row.PinnedProvider,
		Model:          row.PinnedModel,
		Reason:         row.DecisionReason,
		TurnCount:      int(row.TurnCount),
		PinnedUntil:    timestampOrZero(row.PinnedUntil),
		FirstPinnedAt:  timestampOrZero(row.FirstPinnedAt),
		LastSeenAt:     timestampOrZero(row.LastSeenAt),
	}
	// Bounded copy guards against a corrupt row panicking the request handler.
	copy(pin.SessionKey[:], row.SessionKey)
	return pin
}
