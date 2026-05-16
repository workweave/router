package postgres

import (
	"context"
	"math"
	"time"

	"workweave/router/internal/proxy"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// usdPerMicro converts BIGINT micros (USD x 1e6) back to a float64 USD value
// at the adapter boundary. The router's in-Go cost math is float64 USD;
// micros is the storage representation only.
const usdPerMicro = 1.0 / 1_000_000.0

// usdToMicros rounds a float64 USD value to BIGINT micros (USD x 1e6) for
// persistence. NaN/Inf collapse to 0 — we never want to write nonsense.
func usdToMicros(f float64) int64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return int64(math.Round(f * 1_000_000))
}

// microsToUSD is the inverse of usdToMicros, used when projecting stored
// telemetry rows back into the proxy domain types.
func microsToUSD(micros int64) float64 {
	return float64(micros) * usdPerMicro
}

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
		RequestedInputCostUsd:  usdToMicros(p.RequestedInputCostUSD),
		RequestedOutputCostUsd: usdToMicros(p.RequestedOutputCostUSD),
		ActualInputCostUsd:     usdToMicros(p.ActualInputCostUSD),
		ActualOutputCostUsd:    usdToMicros(p.ActualOutputCostUSD),
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
		TotalRequestedCostUSD: microsToUSD(row.TotalRequestedCostUsd),
		TotalActualCostUSD:    microsToUSD(row.TotalActualCostUsd),
		TotalSavingsUSD:       microsToUSD(row.TotalSavingsUsd),
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
				RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
				ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
				RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
				ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
			RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
			ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
		TotalRequestedCostUSD: microsToUSD(row.TotalRequestedCostUsd),
		TotalActualCostUSD:    microsToUSD(row.TotalActualCostUsd),
		TotalSavingsUSD:       microsToUSD(row.TotalSavingsUsd),
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
				RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
				ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
				RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
				ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
			RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
			ActualCostUSD:    microsToUSD(row.ActualCostUsd),
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
	requestedCostUsdMicros int64,
	actualCostUsdMicros int64,
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
		RequestedCostUSD:    microsToUSD(requestedCostUsdMicros),
		ActualCostUSD:       microsToUSD(actualCostUsdMicros),
		TotalLatencyMs:      derefInt64(totalLatencyMs),
		UpstreamStatusCode:  derefInt32(upstreamStatusCode),
	}
}
