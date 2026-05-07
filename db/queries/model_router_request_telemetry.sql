-- Records a completed proxied request for the dashboard UI.
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
    upstream_status_code
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
    @requested_input_cost_usd::numeric,
    @requested_output_cost_usd::numeric,
    @actual_input_cost_usd::numeric,
    @actual_output_cost_usd::numeric,
    @route_latency_ms::bigint,
    @upstream_latency_ms::bigint,
    @total_latency_ms::bigint,
    @cross_format::boolean,
    @upstream_status_code::int
)
ON CONFLICT (installation_id, request_id, span_type) DO NOTHING;

-- Returns aggregated cost and token totals across every installation.
-- Used by admin-cookie sessions on the dashboard, which are not scoped to
-- a single rk_ key.
-- name: GetTelemetrySummaryAll :one
SELECT
    COUNT(*)::bigint                                            AS request_count,
    COALESCE(SUM(input_tokens + output_tokens), 0)::bigint     AS total_tokens,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS total_requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS total_actual_cost_usd,
    COALESCE(SUM(
        (requested_input_cost_usd + requested_output_cost_usd) -
        (actual_input_cost_usd + actual_output_cost_usd)
    ), 0)::numeric                                             AS total_savings_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz;

-- Per-hour cost buckets across every installation. Admin-only.
-- name: GetTelemetryTimeseriesHourlyAll :many
SELECT
    date_trunc('hour', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS actual_cost_usd
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
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('day', timestamp)
ORDER BY bucket ASC;

-- Returns aggregated cost and token totals for the dashboard cards.
-- name: GetTelemetrySummary :one
SELECT
    COUNT(*)::bigint                                            AS request_count,
    COALESCE(SUM(input_tokens + output_tokens), 0)::bigint     AS total_tokens,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS total_requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS total_actual_cost_usd,
    COALESCE(SUM(
        (requested_input_cost_usd + requested_output_cost_usd) -
        (actual_input_cost_usd + actual_output_cost_usd)
    ), 0)::numeric                                             AS total_savings_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz;

-- Returns per-hour cost buckets for the cost savings chart.
-- name: GetTelemetryTimeseriesHourly :many
SELECT
    date_trunc('hour', timestamp)::timestamptz                                       AS bucket,
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS actual_cost_usd
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
    COALESCE(SUM(requested_input_cost_usd + requested_output_cost_usd), 0)::numeric AS requested_cost_usd,
    COALESCE(SUM(actual_input_cost_usd + actual_output_cost_usd), 0)::numeric       AS actual_cost_usd
FROM router.model_router_request_telemetry
WHERE installation_id = @installation_id::uuid
  AND span_type = 'router.upstream'
  AND timestamp >= @from_time::timestamptz
  AND timestamp < @to_time::timestamptz
GROUP BY date_trunc('day', timestamp)
ORDER BY bucket ASC;
