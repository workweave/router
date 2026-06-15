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
// request. Both reads are best-effort: an absent telemetry row (telemetry
// disabled/pruned, or still in flight via async fireTelemetry) and an absent
// feedback row are not errors, so a saved rating is still returned even when
// the routing context is missing, and vice versa.
func (r *FeedbackRepo) GetContext(ctx context.Context, installationID, requestID string) (proxy.FeedbackContext, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return proxy.FeedbackContext{}, err
	}
	q := sqlc.New(r.tx)

	out := proxy.FeedbackContext{RequestID: requestID}

	// Routing context is best-effort: its absence must not hide a saved rating,
	// so ErrNoRows here is not fatal — we just leave the context fields empty.
	tele, err := q.GetRequestForFeedback(ctx, sqlc.GetRequestForFeedbackParams{
		InstallationID: id,
		RequestID:      requestID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return proxy.FeedbackContext{}, err
	}
	if err == nil {
		out.ChosenModel = derefString(tele.DecisionModel)
		out.ChosenProvider = derefString(tele.DecisionProvider)
		out.ClientApp = derefString(tele.ClientApp)
		out.RoutedAt = tele.Timestamp.Time
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
