package postgres

import (
	"context"
	"math"
	"math/big"
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
		ClusterIds:             p.ClusterIDs,
		CandidateModels:        p.CandidateModels,
		ChosenScore:            p.ChosenScore,
		AlphaBreakdown:         p.AlphaBreakdown,
		ClusterRouterVersion:   stringPtrOrNil(p.ClusterRouterVersion),
		TtftMs:                 p.TTFTMs,
		CacheCreationTokens:    p.CacheCreationTokens,
		CacheReadTokens:        p.CacheReadTokens,
		DeviceID:               stringPtrOrNil(p.DeviceID),
		SessionID:              stringPtrOrNil(p.SessionID),
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

	switch granularity {
	case "week":
		rows, err := q.GetTelemetryTimeseriesWeekly(ctx, sqlc.GetTelemetryTimeseriesWeeklyParams{
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
	case "day":
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

// GetTelemetrySummaryAll aggregates across every installation for the admin dashboard.
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

// GetTelemetryTimeseriesAll is the admin-only counterpart to GetTelemetryTimeseries.
func (r *TelemetryRepo) GetTelemetryTimeseriesAll(ctx context.Context, from, to time.Time, granularity string) ([]proxy.TelemetryBucket, error) {
	q := sqlc.New(r.tx)

	switch granularity {
	case "week":
		rows, err := q.GetTelemetryTimeseriesWeeklyAll(ctx, sqlc.GetTelemetryTimeseriesWeeklyAllParams{
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
	case "day":
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

// GetTelemetryRows returns individual telemetry rows for an installation in [from, to) for the drill-down modal.
func (r *TelemetryRepo) GetTelemetryRows(ctx context.Context, installationID string, from, to time.Time, limit int32) ([]proxy.TelemetryRow, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	rows, err := q.GetTelemetryRows(ctx, sqlc.GetTelemetryRowsParams{
		InstallationID: id,
		FromTime:       pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:         pgtype.Timestamptz{Time: to, Valid: true},
		RowLimit:       limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]proxy.TelemetryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, telemetryRowFromRow(
			row.Timestamp.Time,
			row.RequestID,
			row.RequestedModel,
			row.DecisionModel,
			row.DecisionProvider,
			row.DecisionReason,
			row.StickyHit,
			row.InputTokens,
			row.OutputTokens,
			row.CacheCreationTokens,
			row.CacheReadTokens,
			row.RequestedCostUsd,
			row.ActualCostUsd,
			row.TotalLatencyMs,
			row.UpstreamStatusCode,
		))
	}
	return out, nil
}

// GetTelemetryRowsAll is the admin-only counterpart to GetTelemetryRows.
func (r *TelemetryRepo) GetTelemetryRowsAll(ctx context.Context, from, to time.Time, limit int32) ([]proxy.TelemetryRow, error) {
	q := sqlc.New(r.tx)
	rows, err := q.GetTelemetryRowsAll(ctx, sqlc.GetTelemetryRowsAllParams{
		FromTime: pgtype.Timestamptz{Time: from, Valid: true},
		ToTime:   pgtype.Timestamptz{Time: to, Valid: true},
		RowLimit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]proxy.TelemetryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, telemetryRowFromRow(
			row.Timestamp.Time,
			row.RequestID,
			row.RequestedModel,
			row.DecisionModel,
			row.DecisionProvider,
			row.DecisionReason,
			row.StickyHit,
			row.InputTokens,
			row.OutputTokens,
			row.CacheCreationTokens,
			row.CacheReadTokens,
			row.RequestedCostUsd,
			row.ActualCostUsd,
			row.TotalLatencyMs,
			row.UpstreamStatusCode,
		))
	}
	return out, nil
}

// telemetryRowFromRow centralizes SQLC -> domain conversion. The {all, per-installation}
// queries emit isomorphic but distinctly named row types, so we accept individual fields.
func telemetryRowFromRow(
	ts time.Time,
	requestID string,
	requestedModel *string,
	decisionModel *string,
	decisionProvider *string,
	decisionReason *string,
	stickyHit *bool,
	inputTokens *int32,
	outputTokens *int32,
	cacheCreationTokens *int32,
	cacheReadTokens *int32,
	requestedCostUsd pgtype.Numeric,
	actualCostUsd pgtype.Numeric,
	totalLatencyMs *int64,
	upstreamStatusCode *int32,
) proxy.TelemetryRow {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	derefBool := func(b *bool) bool { return b != nil && *b }
	derefInt32 := func(i *int32) int32 {
		if i == nil {
			return 0
		}
		return *i
	}
	derefInt64 := func(i *int64) int64 {
		if i == nil {
			return 0
		}
		return *i
	}
	return proxy.TelemetryRow{
		Timestamp:           ts,
		RequestID:           requestID,
		RequestedModel:      deref(requestedModel),
		DecisionModel:       deref(decisionModel),
		DecisionProvider:    deref(decisionProvider),
		DecisionReason:      deref(decisionReason),
		StickyHit:           derefBool(stickyHit),
		InputTokens:         derefInt32(inputTokens),
		OutputTokens:        derefInt32(outputTokens),
		CacheCreationTokens: cacheCreationTokens,
		CacheReadTokens:     cacheReadTokens,
		RequestedCostUSD:    numericToFloat(requestedCostUsd),
		ActualCostUSD:       numericToFloat(actualCostUsd),
		TotalLatencyMs:      derefInt64(totalLatencyMs),
		UpstreamStatusCode:  derefInt32(upstreamStatusCode),
	}
}

// toNumeric converts a float64 cost value to pgtype.Numeric.
//
// pgtype.Numeric.Scan only accepts string/[]byte/nil — passing a float64
// returns an error which silently leaves the value Valid:false, so every
// cost ended up persisting as NULL (→ DEFAULT 0). Build the Numeric by
// hand from the float's mantissa/exponent at fixed scale instead.
func toNumeric(f float64) pgtype.Numeric {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return pgtype.Numeric{Valid: false}
	}
	// Cost columns are NUMERIC(16,6). Match that scale: multiply by 1e6,
	// round to int, store with Exp = -6.
	const scale = 6
	scaled := math.Round(f * 1e6)
	return pgtype.Numeric{
		Int:   new(big.Int).SetInt64(int64(scaled)),
		Exp:   -scale,
		Valid: true,
	}
}

// numericToFloat converts a pgtype.Numeric to float64, returning 0 on failure.
// Uses Float64Value because Scan silently no-ops on a *float64 destination.
func numericToFloat(n pgtype.Numeric) float64 {
	if !n.Valid {
		return 0
	}
	f8, err := n.Float64Value()
	if err != nil || !f8.Valid {
		return 0
	}
	return f8.Float64
}
