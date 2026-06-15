package postgres

import (
	"context"
	"errors"

	"workweave/router/internal/proxy"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FeedbackRepo implements proxy.FeedbackRepository via SQLC. It owns
// router.request_feedback and reads routing context from
// router.model_router_request_telemetry to render the feedback page.
type FeedbackRepo struct {
	tx sqlc.DBTX
}

// NewFeedbackRepo constructs a FeedbackRepo backed by the given connection.
func NewFeedbackRepo(tx sqlc.DBTX) *FeedbackRepo {
	return &FeedbackRepo{tx: tx}
}

var _ proxy.FeedbackRepository = (*FeedbackRepo)(nil)

// Upsert writes (or revises) one request's feedback row.
func (r *FeedbackRepo) Upsert(ctx context.Context, p proxy.UpsertFeedbackParams) error {
	installationID, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	source := p.Source
	if source == "" {
		source = "link"
	}
	q := sqlc.New(r.tx)
	return q.UpsertRequestFeedback(ctx, sqlc.UpsertRequestFeedbackParams{
		InstallationID: installationID,
		ExternalID:     p.ExternalID,
		RequestID:      p.RequestID,
		Rating:         p.Rating,
		Comment:        p.Comment,
		Source:         source,
		RouterUserID:   uuidOrNil(p.RouterUserID),
	})
}

// GetContext returns the routing context plus any existing feedback for a
// request. Returns pgx.ErrNoRows when the request id is unknown for the
// installation (no telemetry row); an absent feedback row is not an error.
func (r *FeedbackRepo) GetContext(ctx context.Context, installationID, requestID string) (proxy.FeedbackContext, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return proxy.FeedbackContext{}, err
	}
	q := sqlc.New(r.tx)

	tele, err := q.GetRequestForFeedback(ctx, sqlc.GetRequestForFeedbackParams{
		InstallationID: id,
		RequestID:      requestID,
	})
	if err != nil {
		return proxy.FeedbackContext{}, err
	}

	out := proxy.FeedbackContext{
		RequestID:      requestID,
		ChosenModel:    derefString(tele.DecisionModel),
		ChosenProvider: derefString(tele.DecisionProvider),
		ClientApp:      derefString(tele.ClientApp),
		RoutedAt:       tele.Timestamp.Time,
	}

	fb, err := q.GetRequestFeedback(ctx, sqlc.GetRequestFeedbackParams{
		InstallationID: id,
		RequestID:      requestID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return proxy.FeedbackContext{}, err
	}
	if err == nil {
		out.Rating = fb.Rating
		out.Comment = fb.Comment
	}
	return out, nil
}

// derefString returns the pointed-to string or "" for a nil pointer.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
