package postgres

import (
	"context"
	"time"

	"workweave/router/internal/proxy"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TelemetryRepo implements proxy.TelemetryRepository via SQLC.
type TelemetryRepo struct {
	tx sqlc.DBTX
}

// NewTelemetryRepo constructs a TelemetryRepo backed by the given connection.
func NewTelemetryRepo(tx sqlc.DBTX) *TelemetryRepo {
	return &TelemetryRepo{tx: tx}
}

// Compile-time interface check.
var _ proxy.TelemetryRepository = (*TelemetryRepo)(nil)

func (r *TelemetryRepo) InsertRequestTelemetry(ctx context.Context, p proxy.InsertTelemetryParams) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertRequestTelemetry(ctx, sqlc.InsertRequestTelemetryParams{
		InstallationID:         id,
		RequestID:              p.RequestID,
		SpanType:               p.SpanType,
		TraceID:                p.TraceID,
		Timestamp:              pgtype.Timestamptz{Time: p.Timestamp, Valid: true},
		RequestedModel:         p.RequestedModel,
		DecisionModel:          p.DecisionModel,
		DecisionProvider:       p.DecisionProvider,
		DecisionReason:         p.DecisionReason,
		EstimatedInputTokens:   p.EstimatedInputTokens,
		StickyHit:              p.StickyHit,
		EmbedInput:             p.EmbedInput,
		InputTokens:            p.InputTokens,
		OutputTokens:           p.OutputTokens,
		RequestedInputCostUsd:  toNumeric(p.RequestedInputCostUSD),
		RequestedOutputCostUsd: toNumeric(p.RequestedOutputCostUSD),
		ActualInputCostUsd:     toNumeric(p.ActualInputCostUSD),
		ActualOutputCostUsd:    toNumeric(p.ActualOutputCostUSD),
		RouteLatencyMs:         p.RouteLatencyMs,
		UpstreamLatencyMs:      p.UpstreamLatencyMs,
		TotalLatencyMs:         p.TotalLatencyMs,
		CrossFormat:            p.CrossFormat,
		UpstreamStatusCode:     p.UpstreamStatusCode,
	})
}

func (r *TelemetryRepo) GetTelemetrySummary(ctx context.Context, installationID string, from, to time.Time) (proxy.TelemetrySummary, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return proxy.TelemetrySummary{}, err
	}
	q := sqlc.New(r.tx)
	row, err := q.GetTelemetrySummary(ctx, sqlc.GetTelemetrySummaryParams{
		InstallationID: id,
		FromTime:       pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:         pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return proxy.TelemetrySummary{}, err
	}
	return proxy.TelemetrySummary{
		RequestCount:          row.RequestCount,
		TotalTokens:           row.TotalTokens,
		TotalRequestedCostUSD: numericToFloat(row.TotalRequestedCostUsd),
		TotalActualCostUSD:    numericToFloat(row.TotalActualCostUsd),
		TotalSavingsUSD:       numericToFloat(row.TotalSavingsUsd),
	}, nil
}

func (r *TelemetryRepo) GetTelemetryTimeseries(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]proxy.TelemetryBucket, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)

	if granularity == "day" {
		rows, err := q.GetTelemetryTimeseriesDaily(ctx, sqlc.GetTelemetryTimeseriesDailyParams{
			InstallationID: id,
			FromTime:       pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:         pgtype.Timestamptz{Time: to, Valid: true},
		})
		if err != nil {
			return nil, err
		}
		out := make([]proxy.TelemetryBucket, 0, len(rows))
		for _, row := range rows {
			out = append(out, proxy.TelemetryBucket{
				Bucket:           row.Bucket.Time,
				RequestedCostUSD: numericToFloat(row.RequestedCostUsd),
				ActualCostUSD:    numericToFloat(row.ActualCostUsd),
			})
		}
		return out, nil
	}

	rows, err := q.GetTelemetryTimeseriesHourly(ctx, sqlc.GetTelemetryTimeseriesHourlyParams{
		InstallationID: id,
		FromTime:       pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:         pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := make([]proxy.TelemetryBucket, 0, len(rows))
	for _, row := range rows {
		out = append(out, proxy.TelemetryBucket{
			Bucket:           row.Bucket.Time,
			RequestedCostUSD: numericToFloat(row.RequestedCostUsd),
			ActualCostUSD:    numericToFloat(row.ActualCostUsd),
		})
	}
	return out, nil
}

// GetTelemetrySummaryAll aggregates across every installation. Used by the
// admin dashboard, which is not scoped to a single rk_ key.
func (r *TelemetryRepo) GetTelemetrySummaryAll(ctx context.Context, from, to time.Time) (proxy.TelemetrySummary, error) {
	q := sqlc.New(r.tx)
	row, err := q.GetTelemetrySummaryAll(ctx, sqlc.GetTelemetrySummaryAllParams{
		FromTime: pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return proxy.TelemetrySummary{}, err
	}
	return proxy.TelemetrySummary{
		RequestCount:          row.RequestCount,
		TotalTokens:           row.TotalTokens,
		TotalRequestedCostUSD: numericToFloat(row.TotalRequestedCostUsd),
		TotalActualCostUSD:    numericToFloat(row.TotalActualCostUsd),
		TotalSavingsUSD:       numericToFloat(row.TotalSavingsUsd),
	}, nil
}

// GetTelemetryTimeseriesAll returns per-bucket cost rows aggregated across
// every installation. Admin-only counterpart to GetTelemetryTimeseries.
func (r *TelemetryRepo) GetTelemetryTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]proxy.TelemetryBucket, error) {
	q := sqlc.New(r.tx)

	if granularity == "day" {
		rows, err := q.GetTelemetryTimeseriesDailyAll(ctx, sqlc.GetTelemetryTimeseriesDailyAllParams{
			FromTime: pgtype.Timestamptz{Time: from, Valid: true},
			ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
		})
		if err != nil {
			return nil, err
		}
		out := make([]proxy.TelemetryBucket, 0, len(rows))
		for _, row := range rows {
			out = append(out, proxy.TelemetryBucket{
				Bucket:           row.Bucket.Time,
				RequestedCostUSD: numericToFloat(row.RequestedCostUsd),
				ActualCostUSD:    numericToFloat(row.ActualCostUsd),
			})
		}
		return out, nil
	}

	rows, err := q.GetTelemetryTimeseriesHourlyAll(ctx, sqlc.GetTelemetryTimeseriesHourlyAllParams{
		FromTime: pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	out := make([]proxy.TelemetryBucket, 0, len(rows))
	for _, row := range rows {
		out = append(out, proxy.TelemetryBucket{
			Bucket:           row.Bucket.Time,
			RequestedCostUSD: numericToFloat(row.RequestedCostUsd),
			ActualCostUSD:    numericToFloat(row.ActualCostUsd),
		})
	}
	return out, nil
}

// toNumeric converts a float64 cost value to pgtype.Numeric via string round-trip.
func toNumeric(f float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(f)
	return n
}

// numericToFloat converts a pgtype.Numeric to float64, returning 0 on failure.
func numericToFloat(n pgtype.Numeric) float64 {
	if !n.Valid {
		return 0
	}
	var f float64
	if err := n.Scan(&f); err != nil {
		return 0
	}
	return f
}
