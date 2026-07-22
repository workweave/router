package postgres

import (
	"context"
	"time"

	"workweave/router/internal/proxy"
	"workweave/router/internal/router/catalog"
	"workweave/router/internal/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// usdPerMicro converts BIGINT micros (USD x 1e6) back to a float64 USD value
// at the adapter boundary. The router's in-Go cost math is float64 USD;
// micros is the storage representation only.
const usdPerMicro = 1.0 / 1_000_000.0

// microsToUSD is the inverse of catalog.USDToMicros, used when projecting
// stored telemetry rows back into the proxy domain types.
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
var _ proxy.PolicyShadowStore = (*TelemetryRepo)(nil)

func (r *TelemetryRepo) InsertPolicyShadowDecision(ctx context.Context, p proxy.PolicyShadowDecision) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertPolicyShadowDecision(ctx, sqlc.InsertPolicyShadowDecisionParams{
		InstallationID:              id,
		OrganizationID:              stringPtrOrNil(p.OrganizationID),
		RolloutID:                   stringPtrOrNil(p.RolloutID),
		ClientApp:                   stringPtrOrNil(p.ClientApp),
		TrainingAllowed:             p.TrainingAllowed,
		ServingStrategy:             p.ServingStrategy,
		ServingModel:                p.ServingModel,
		ServingProvider:             p.ServingProvider,
		ServingRouteID:              stringPtrOrNil(p.ServingRouteID),
		ServingPolicyRouteKey:       stringPtrOrNil(p.ServingPolicyRouteKey),
		ServingPolicyArtifactID:     stringPtrOrNil(p.ServingPolicyArtifactID),
		ServingPolicyArtifactSha256: stringPtrOrNil(p.ServingPolicyArtifactSHA256),
		ShadowStrategy:              p.ShadowStrategy,
		ShadowModel:                 stringPtrOrNil(p.ShadowModel),
		ShadowProvider:              stringPtrOrNil(p.ShadowProvider),
		ShadowRouteID:               stringPtrOrNil(p.ShadowRouteID),
		ShadowPolicyRouteKey:        stringPtrOrNil(p.ShadowPolicyRouteKey),
		ShadowPolicyArtifactID:      stringPtrOrNil(p.ShadowPolicyArtifactID),
		ShadowPolicyArtifactSha256:  stringPtrOrNil(p.ShadowPolicyArtifactSHA256),
		ShadowLatencyMs:             p.ShadowLatencyMs,
		ShadowError:                 stringPtrOrNil(p.ShadowError),
		ModelsAgree:                 p.ModelsAgree,
	})
}

func (r *TelemetryRepo) InsertRequestTelemetry(ctx context.Context, p proxy.InsertTelemetryParams) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertRequestTelemetry(ctx, sqlc.InsertRequestTelemetryParams{
		InstallationID:         id,
		APIKeyID:               uuidOrNil(p.APIKeyID),
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
		RequestedInputCostUsd:  catalog.USDToMicros(p.RequestedInputCostUSD),
		RequestedOutputCostUsd: catalog.USDToMicros(p.RequestedOutputCostUSD),
		ActualInputCostUsd:     catalog.USDToMicros(p.ActualInputCostUSD),
		ActualOutputCostUsd:    catalog.USDToMicros(p.ActualOutputCostUSD),
		RouteLatencyMs:         p.RouteLatencyMs,
		UpstreamLatencyMs:      p.UpstreamLatencyMs,
		TotalLatencyMs:         p.TotalLatencyMs,
		CrossFormat:            p.CrossFormat,
		UpstreamStatusCode:     p.UpstreamStatusCode,
		ClusterIds:             p.ClusterIDs,
		CandidateModels:        p.CandidateModels,
		ChosenScore:            p.ChosenScore,
		CandidateScores:        p.CandidateScores,
		Propensity:             p.Propensity,
		AlphaBreakdown:         p.AlphaBreakdown,
		ClusterRouterVersion:   stringPtrOrNil(p.ClusterRouterVersion),
		Strategy:               stringPtrOrNil(p.Strategy),
		RouteID:                stringPtrOrNil(p.RouteID),
		PolicyRouteKey:         stringPtrOrNil(p.PolicyRouteKey),
		PolicyArtifactID:       stringPtrOrNil(p.PolicyArtifactID),
		PolicyArtifactSha256:   stringPtrOrNil(p.PolicyArtifactSHA256),
		RosterVersion:          stringPtrOrNil(p.RosterVersion),
		SidecarSchemaVersion:   stringPtrOrNil(p.SidecarSchemaVersion),
		TrainingAllowed:        p.TrainingAllowed,
		CaptureMode:            p.CaptureMode,
		DebugRef:               stringPtrOrNil(p.DebugRef),
		TtftMs:                 p.TTFTMs,
		CacheCreationTokens:    p.CacheCreationTokens,
		CacheReadTokens:        p.CacheReadTokens,
		DeviceID:               stringPtrOrNil(p.DeviceID),
		SessionID:              stringPtrOrNil(p.SessionID),
		RouterUserID:           uuidOrNil(p.RouterUserID),
		ClientApp:              stringPtrOrNil(p.ClientApp),
		RolloutID:              stringPtrOrNil(p.RolloutID),
		TurnType:               p.TurnType,
		UpstreamFinishReason:   p.UpstreamFinishReason,
		StopReason:             p.StopReason,
		ToolUseBlocks:          p.ToolUseBlocks,
		InvalidToolArgsBlocks:  p.InvalidToolArgsBlocks,
		FailoverUsed:           p.FailoverUsed,
		DegenerateShadow:       p.DegenerateShadow,
		SessionKey:             p.SessionKey,
		Role:                   stringPtrOrNil(p.Role),
		FreshDecisionModel:     stringPtrOrNil(p.FreshDecisionModel),
		FreshCandidateScores:   p.FreshCandidateScores,
		PinAgeSec:              p.PinAgeSec,
		ToolResultBytes:        p.ToolResultBytes,
		CredentialKeyPrefix:    stringPtrOrNil(p.CredentialKeyPrefix),
		CredentialKeySuffix:    stringPtrOrNil(p.CredentialKeySuffix),
		CredentialSource:       stringPtrOrNil(p.CredentialSource),
		UnifiedLimitHeaders:    p.UnifiedLimitHeaders,
	})
}

var _ proxy.LoopEscalationStore = (*TelemetryRepo)(nil)

func (r *TelemetryRepo) InsertLoopEscalationEvent(ctx context.Context, p proxy.LoopEscalationEvent) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertLoopEscalationEvent(ctx, sqlc.InsertLoopEscalationEventParams{
		InstallationID:   id,
		SessionKey:       p.SessionKey,
		Role:             p.Role,
		LoopingModel:     p.LoopingModel,
		Action:           p.Action,
		EscalationTarget: p.EscalationTarget,
		LoopTool:         p.LoopTool,
		LoopInputHash:    p.LoopInputHash,
		RepeatCount:      p.RepeatCount,
		DistinctRatio:    p.DistinctRatio,
		WindowSize:       p.WindowSize,
	})
}

func (r *TelemetryRepo) CountLoopEscalationEvents(ctx context.Context, sessionKey []byte, role string) (count int64, err error) {
	q := sqlc.New(r.tx)
	return q.CountLoopEscalationEvents(ctx, sqlc.CountLoopEscalationEventsParams{
		SessionKey: sessionKey,
		Role:       role,
	})
}

var _ proxy.RouterFeedbackStore = (*TelemetryRepo)(nil)

func (r *TelemetryRepo) InsertRouterFeedback(ctx context.Context, p proxy.RouterFeedbackEvent) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertRouterFeedback(ctx, sqlc.InsertRouterFeedbackParams{
		InstallationID: id,
		SessionKey:     p.SessionKey,
		Role:           p.Role,
		RouterUserID:   uuidOrNil(p.RouterUserID),
		ClientApp:      stringPtrOrNil(p.ClientApp),
		SessionID:      stringPtrOrNil(p.SessionID),
		RequestedModel: p.RequestedModel,
		ServedModel:    p.ServedModel,
		Rating:         stringPtrOrNil(p.Rating),
		SuggestedLabel: stringPtrOrNil(p.SuggestedLabel),
		Feedback:       p.Feedback,
		Source:         p.Source,
		RequestID:      stringPtrOrNil(p.RequestID),
		RouteID:        stringPtrOrNil(p.RouteID),
	})
}

var _ proxy.SpiralShadowStore = (*TelemetryRepo)(nil)

func (r *TelemetryRepo) InsertSpiralShadowEvent(ctx context.Context, p proxy.SpiralShadowEvent) error {
	id, err := uuid.Parse(p.InstallationID)
	if err != nil {
		return err
	}
	q := sqlc.New(r.tx)
	return q.InsertSpiralShadowEvent(ctx, sqlc.InsertSpiralShadowEventParams{
		InstallationID:   id,
		SessionKey:       p.SessionKey,
		Role:             p.Role,
		RoutedModel:      p.RoutedModel,
		TurnType:         p.TurnType,
		Reason:           p.Reason,
		ErrStreak:        p.ErrStreak,
		ErroredResults:   p.ErroredResults,
		ToolResults:      p.ToolResults,
		MaxSameFileEdits: p.MaxSameFileEdits,
		SameFilePathHash: p.SameFilePathHash,
		RepeatFrac:       p.RepeatFrac,
		MonologueLen:     p.MonologueLen,
		ToolCallCount:    p.ToolCallCount,
		MessageCount:     p.MessageCount,
	})
}

func (r *TelemetryRepo) CountSpiralShadowEvents(ctx context.Context, sessionKey []byte, role, reason string) (count int64, err error) {
	q := sqlc.New(r.tx)
	return q.CountSpiralShadowEvents(ctx, sqlc.CountSpiralShadowEventsParams{
		SessionKey: sessionKey,
		Role:       role,
		Reason:     reason,
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
	fromTs := pgtype.Timestamptz{Time: from, Valid: true}
	toTs := pgtype.Timestamptz{Time: to, Valid: true}

	return selectTimeseriesGranularity(granularity,
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesWeekly(ctx, sqlc.GetTelemetryTimeseriesWeeklyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromWeeklyRow), nil
		},
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesDaily(ctx, sqlc.GetTelemetryTimeseriesDailyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromDailyRow), nil
		},
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesHourly(ctx, sqlc.GetTelemetryTimeseriesHourlyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromHourlyRow), nil
		},
	)
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
	fromTs := pgtype.Timestamptz{Time: from, Valid: true}
	toTs := pgtype.Timestamptz{Time: to, Valid: true}

	return selectTimeseriesGranularity(granularity,
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesWeeklyAll(ctx, sqlc.GetTelemetryTimeseriesWeeklyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromWeeklyAllRow), nil
		},
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesDailyAll(ctx, sqlc.GetTelemetryTimeseriesDailyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromDailyAllRow), nil
		},
		func() ([]proxy.TelemetryBucket, error) {
			rows, err := q.GetTelemetryTimeseriesHourlyAll(ctx, sqlc.GetTelemetryTimeseriesHourlyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, telemetryBucketFromHourlyAllRow), nil
		},
	)
}

// GetTelemetryModelBreakdown returns per-bucket totals grouped by decision
// model for one installation, powering the per-model usage and spend charts.
func (r *TelemetryRepo) GetTelemetryModelBreakdown(ctx context.Context, installationID string, from, to time.Time, granularity string) ([]proxy.TelemetryModelBucket, error) {
	id, err := uuid.Parse(installationID)
	if err != nil {
		return nil, err
	}
	q := sqlc.New(r.tx)
	fromTs := pgtype.Timestamptz{Time: from, Valid: true}
	toTs := pgtype.Timestamptz{Time: to, Valid: true}

	return selectModelBreakdownGranularity(granularity,
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownWeekly(ctx, sqlc.GetTelemetryModelBreakdownWeeklyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromWeeklyRow), nil
		},
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownDaily(ctx, sqlc.GetTelemetryModelBreakdownDailyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromDailyRow), nil
		},
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownHourly(ctx, sqlc.GetTelemetryModelBreakdownHourlyParams{
				InstallationID: id,
				FromTime:       fromTs,
				ToTime:         toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromHourlyRow), nil
		},
	)
}

// GetTelemetryModelBreakdownAll is the admin-only counterpart to
// GetTelemetryModelBreakdown, spanning every installation.
func (r *TelemetryRepo) GetTelemetryModelBreakdownAll(ctx context.Context, from, to time.Time, granularity string) ([]proxy.TelemetryModelBucket, error) {
	q := sqlc.New(r.tx)
	fromTs := pgtype.Timestamptz{Time: from, Valid: true}
	toTs := pgtype.Timestamptz{Time: to, Valid: true}

	return selectModelBreakdownGranularity(granularity,
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownWeeklyAll(ctx, sqlc.GetTelemetryModelBreakdownWeeklyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromWeeklyAllRow), nil
		},
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownDailyAll(ctx, sqlc.GetTelemetryModelBreakdownDailyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromDailyAllRow), nil
		},
		func() ([]proxy.TelemetryModelBucket, error) {
			rows, err := q.GetTelemetryModelBreakdownHourlyAll(ctx, sqlc.GetTelemetryModelBreakdownHourlyAllParams{
				FromTime: fromTs,
				ToTime:   toTs,
			})
			if err != nil {
				return nil, err
			}
			return mapRows(rows, modelBucketFromHourlyAllRow), nil
		},
	)
}

// selectModelBreakdownGranularity dispatches to the query closure matching
// granularity, defaulting to hourly — the model-breakdown counterpart of
// selectTimeseriesGranularity.
func selectModelBreakdownGranularity(
	granularity string,
	weekly, daily, hourly func() ([]proxy.TelemetryModelBucket, error),
) ([]proxy.TelemetryModelBucket, error) {
	switch granularity {
	case "week":
		return weekly()
	case "day":
		return daily()
	default:
		return hourly()
	}
}

// selectTimeseriesGranularity dispatches to the query closure matching
// granularity, defaulting to hourly — the shared switch behind both
// GetTelemetryTimeseries and GetTelemetryTimeseriesAll.
func selectTimeseriesGranularity(
	granularity string,
	weekly, daily, hourly func() ([]proxy.TelemetryBucket, error),
) ([]proxy.TelemetryBucket, error) {
	switch granularity {
	case "week":
		return weekly()
	case "day":
		return daily()
	default:
		return hourly()
	}
}

// mapRows converts a slice of SQLC rows to a slice of domain values via convert.
func mapRows[T, U any](rows []T, convert func(T) U) []U {
	out := make([]U, 0, len(rows))
	for _, row := range rows {
		out = append(out, convert(row))
	}
	return out
}

func telemetryBucketFromWeeklyRow(row sqlc.GetTelemetryTimeseriesWeeklyRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
}

func telemetryBucketFromDailyRow(row sqlc.GetTelemetryTimeseriesDailyRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
}

func telemetryBucketFromHourlyRow(row sqlc.GetTelemetryTimeseriesHourlyRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
}

func telemetryBucketFromWeeklyAllRow(row sqlc.GetTelemetryTimeseriesWeeklyAllRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
}

func telemetryBucketFromDailyAllRow(row sqlc.GetTelemetryTimeseriesDailyAllRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
}

func telemetryBucketFromHourlyAllRow(row sqlc.GetTelemetryTimeseriesHourlyAllRow) proxy.TelemetryBucket {
	return proxy.TelemetryBucket{
		Bucket:           row.Bucket.Time,
		RequestedCostUSD: microsToUSD(row.RequestedCostUsd),
		ActualCostUSD:    microsToUSD(row.ActualCostUsd),
	}
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
	out := mapRows(rows, telemetryRowFromRowsRow)
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
	out := mapRows(rows, telemetryRowFromRowsAllRow)
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
	routerUserID string,
	clientApp *string,
	turnType string,
	userEmail *string,
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
		RouterUserID:        routerUserID,
		ClientApp:           deref(clientApp),
		TurnType:            turnType,
		UserEmail:           deref(userEmail),
	}
}

func telemetryRowFromRowsRow(row sqlc.GetTelemetryRowsRow) proxy.TelemetryRow {
	return telemetryRowFromRow(
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
		uuidString(row.RouterUserID),
		row.ClientApp,
		row.TurnType,
		row.UserEmail,
	)
}

func telemetryRowFromRowsAllRow(row sqlc.GetTelemetryRowsAllRow) proxy.TelemetryRow {
	return telemetryRowFromRow(
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
		uuidString(row.RouterUserID),
		row.ClientApp,
		row.TurnType,
		row.UserEmail,
	)
}

func modelBucketFromWeeklyRow(row sqlc.GetTelemetryModelBreakdownWeeklyRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

func modelBucketFromDailyRow(row sqlc.GetTelemetryModelBreakdownDailyRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

func modelBucketFromHourlyRow(row sqlc.GetTelemetryModelBreakdownHourlyRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

func modelBucketFromWeeklyAllRow(row sqlc.GetTelemetryModelBreakdownWeeklyAllRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

func modelBucketFromDailyAllRow(row sqlc.GetTelemetryModelBreakdownDailyAllRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

func modelBucketFromHourlyAllRow(row sqlc.GetTelemetryModelBreakdownHourlyAllRow) proxy.TelemetryModelBucket {
	return proxy.TelemetryModelBucket{
		Bucket:        row.Bucket.Time,
		DecisionModel: row.DecisionModel,
		RequestCount:  row.RequestCount,
		TotalTokens:   row.TotalTokens,
		ActualCostUSD: microsToUSD(row.ActualCostUsd),
	}
}

// GetTelemetryBySessionSequence returns the N-th main_loop telemetry row for
// (installation_id, session_key, role). seq > 0 = absolute 1-based ASC,
// seq < 0 = relative 1-based DESC. Returns pgx.ErrNoRows when fewer than
// |seq| rows exist.
func (r *TelemetryRepo) GetTelemetryBySessionSequence(ctx context.Context, installationID uuid.UUID, sessionKey []byte, role string, seq int) (proxy.TelemetryTurnResult, error) {
	q := sqlc.New(r.tx)
	if seq > 0 {
		row, err := q.GetTelemetryBySessionAsc(ctx, sqlc.GetTelemetryBySessionAscParams{
			InstallationID: installationID,
			SessionKey:     sessionKey,
			Role:           role,
			TurnOffset:     int32(seq - 1),
		})
		if err != nil {
			return proxy.TelemetryTurnResult{}, err
		}
		return proxy.TelemetryTurnResult{
			RequestID:        row.RequestID,
			DecisionModel:    derefString(row.DecisionModel),
			DecisionProvider: derefString(row.DecisionProvider),
			RouteID:          derefString(row.RouteID),
			Strategy:         derefString(row.Strategy),
			Timestamp:        row.Timestamp.Time,
		}, nil
	}
	row, err := q.GetTelemetryBySessionDesc(ctx, sqlc.GetTelemetryBySessionDescParams{
		InstallationID: installationID,
		SessionKey:     sessionKey,
		Role:           role,
		TurnOffset:     int32(-seq - 1),
	})
	if err != nil {
		return proxy.TelemetryTurnResult{}, err
	}
	return proxy.TelemetryTurnResult{
		RequestID:        row.RequestID,
		DecisionModel:    derefString(row.DecisionModel),
		DecisionProvider: derefString(row.DecisionProvider),
		RouteID:          derefString(row.RouteID),
		Strategy:         derefString(row.Strategy),
		Timestamp:        row.Timestamp.Time,
	}, nil
}
