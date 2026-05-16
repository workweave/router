-- Records a completed proxied request for the dashboard UI and routing
-- observability. Routing-brain fields (cluster_ids, candidate_models,
-- chosen_score, alpha_breakdown, cluster_router_version, ttft_ms,
-- cache_*_tokens, device_id, session_id) are nullable; non-cluster
-- decisions and pinned-route turns leave them NULL.
-- name: InsertRequestTelemetry :exec
INSERT INTO router.model_router_request_telemetry (
    installation_id,
    request_id,
    span_type,
    trace_id,
    timestamp,
    requested_model,
    decision_model,
    decision_provider,
    decision_reason,
    estimated_input_tokens,
    sticky_hit,
    embed_input,
    input_tokens,
    output_tokens,
    requested_input_cost_usd,
    requested_output_cost_usd,
    actual_input_cost_usd,
    actual_output_cost_usd,
    route_latency_ms,
    upstream_latency_ms,
    total_latency_ms,
    cross_format,
    upstream_status_code,
    cluster_ids,
    candidate_models,
    chosen_score,
    alpha_breakdown,
    cluster_router_version,
    ttft_ms,
    cache_creation_tokens,
    cache_read_tokens,
    device_id,
    session_id
) VALUES (
    @installation_id::uuid,
    @request_id::varchar,
    @span_type::varchar,
    @trace_id::varchar,
    @timestamp::timestamptz,
    @requested_model::varchar,
    @decision_model::varchar,
    @decision_provider::varchar,
    @decision_reason::varchar,
    @estimated_input_tokens::int,
    @sticky_hit::boolean,
    @embed_input::varchar,
    @input_tokens::int,
    @output_tokens::int,
    @requested_input_cost_usd::bigint,
    @requested_output_cost_usd::bigint,
    @actual_input_cost_usd::bigint,
    @actual_output_cost_usd::bigint,
    @route_latency_ms::bigint,
    @upstream_latency_ms::bigint,
    @total_latency_ms::bigint,
    @cross_format::boolean,
    @upstream_status_code::int,
    sqlc.narg('cluster_ids')::int[],
    sqlc.narg('candidate_models')::text[],
    sqlc.narg('chosen_score')::double precision,
    sqlc.narg('alpha_breakdown')::jsonb,
    sqlc.narg('cluster_router_version')::varchar,
    sqlc.narg('ttft_ms')::bigint,
    sqlc.narg('cache_creation_tokens')::int,
    sqlc.narg('cache_read_tokens')::int,
    sqlc.narg('device_id')::varchar,
    sqlc.narg('session_id')::varchar
)
ON CONFLICT (installation_id, request_id, span_type) DO NOTHING;

-- Returns aggregated cost and token totals across every installation.
-- Used by admin-cookie sessions on the dashboard, which are not scoped to
-- a single rk_ key.
-- name: GetTelemetrySummaryAll :one
SELECT
    COUNT(*)::bigint                                            AS request_count,
    COALESCE(SUM(input_tokens + output_tokens), 0)::bigint     AS total_tokens,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS total_requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS total_actual_cost_usd,
    COALESCE(SUM(
        (requested_input_cost_usd + requested_output_cost_usd) -
        (actual_input_cost_usd + actual_output_cost_usd)
    ), 0)::bigint                                             AS total_savings_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz;

-- Per-hour cost buckets across every installation. Admin-only.
-- name: GetTelemetryTimeseriesHourlyAll :many
SELECT
    date_trunc('hour', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('hour', timestamp)
ORDER BY bucket ASC;

-- Per-day cost buckets across every installation. Admin-only.
-- name: GetTelemetryTimeseriesDailyAll :many
SELECT
    date_trunc('day', timestamp)::timestamptz                                        AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('day', timestamp)
ORDER BY bucket ASC;

-- Per-ISO-week cost buckets across every installation. Admin-only.
-- name: GetTelemetryTimeseriesWeeklyAll :many
SELECT
    date_trunc('week', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('week', timestamp)
ORDER BY bucket ASC;

-- Returns aggregated cost and token totals for the dashboard cards.
-- name: GetTelemetrySummary :one
SELECT
    COUNT(*)::bigint                                            AS request_count,
    COALESCE(SUM(input_tokens + output_tokens), 0)::bigint     AS total_tokens,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS total_requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS total_actual_cost_usd,
    COALESCE(SUM(
        (requested_input_cost_usd + requested_output_cost_usd) -
        (actual_input_cost_usd + actual_output_cost_usd)
    ), 0)::bigint                                             AS total_savings_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz;

-- Returns per-hour cost buckets for the cost savings chart.
-- name: GetTelemetryTimeseriesHourly :many
SELECT
    date_trunc('hour', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('hour', timestamp)
ORDER BY bucket ASC;

-- Returns per-day cost buckets for the cost savings chart.
-- name: GetTelemetryTimeseriesDaily :many
SELECT
    date_trunc('day', timestamp)::timestamptz                                        AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('day', timestamp)
ORDER BY bucket ASC;

-- Returns per-ISO-week cost buckets for the cost savings chart.
-- name: GetTelemetryTimeseriesWeekly :many
SELECT
    date_trunc('week', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::bigint AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::bigint       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('week', timestamp)
ORDER BY bucket ASC;

-- Returns individual telemetry rows for a time window. Used by the
-- dashboard drill-down modal to show the underlying requests behind a
-- chart bucket. Admin scope: spans every installation.
-- name: GetTelemetryRowsAll :many
SELECT
    timestamp,
    request_id,
    requested_model,
    decision_model,
    decision_provider,
    decision_reason,
    sticky_hit,
    input_tokens,
    output_tokens,
    cache_creation_tokens,
    cache_read_tokens,
    (requested_input_cost_usd + requested_output_cost_usd)::bigint AS requested_cost_usd,
    (actual_input_cost_usd + actual_output_cost_usd)::bigint       AS actual_cost_usd,
    total_latency_ms,
    upstream_status_code
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
ORDER BY timestamp DESC
LIMIT @row_limit::int;

-- Returns individual telemetry rows scoped to a single installation.
-- name: GetTelemetryRows :many
SELECT
    timestamp,
    request_id,
    requested_model,
    decision_model,
    decision_provider,
    decision_reason,
    sticky_hit,
    input_tokens,
    output_tokens,
    cache_creation_tokens,
    cache_read_tokens,
    (requested_input_cost_usd + requested_output_cost_usd)::bigint AS requested_cost_usd,
    (actual_input_cost_usd + actual_output_cost_usd)::bigint       AS actual_cost_usd,
    total_latency_ms,
    upstream_status_code
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
ORDER BY timestamp DESC
LIMIT @row_limit::int;
