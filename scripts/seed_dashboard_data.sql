-- Seed the local router DB with a fake installation + 90 days of hourly
-- telemetry rows so the /dashboard page has data to render.
--
-- Idempotent: re-running upserts the installation and re-inserts a
-- fresh batch of telemetry. The unique index on
-- (installation_id, request_id, span_type) means re-running with the
-- same generate_series window will be a no-op except for any new hours
-- that have elapsed since the last run.
--
-- Usage:
--   PGPASSWORD=router psql -h localhost -p 5433 -U router -d router \
--     -f router/scripts/seed_dashboard_data.sql

BEGIN;

-- 1) Ensure a fake installation exists.
INSERT INTO router.model_router_installations (id, external_id, name, created_by)
VALUES (
    '11111111-1111-1111-1111-111111111111',
    'seed-org',
    'Seed Org',
    'seed-script'
)
ON CONFLICT (external_id, name) WHERE deleted_at IS NULL DO NOTHING;

-- 2) Generate one telemetry row per hour for the past 90 days.
-- Costs are derived from a few deterministic sin-wave-ish patterns so
-- charts have visible structure (peaks during "work hours", a small
-- weekly cycle, and a steady drift up over the 3 months).
WITH hours AS (
    SELECT
        gs                                                AS bucket,
        EXTRACT(EPOCH FROM gs)::bigint                    AS epoch_s,
        EXTRACT(HOUR FROM gs)::int                        AS hod,
        EXTRACT(DOW  FROM gs)::int                        AS dow,
        (NOW() - gs)                                      AS age,
        ROW_NUMBER() OVER (ORDER BY gs)                   AS rn
    FROM generate_series(
        date_trunc('hour', NOW() - INTERVAL '90 days'),
        date_trunc('hour', NOW()),
        INTERVAL '1 hour'
    ) AS gs
),
shaped AS (
    SELECT
        bucket,
        rn,
        -- Daily peak around 14:00, weekend dampening, +20% drift over 90d.
        GREATEST(
            1,
            ROUND(
                (40 + 35 * SIN((hod - 6) * PI() / 12.0))   -- diurnal
                * (CASE WHEN dow IN (0, 6) THEN 0.55 ELSE 1.0 END)
                * (1.0 + 0.20 * (1 - EXTRACT(EPOCH FROM age) / EXTRACT(EPOCH FROM INTERVAL '90 days')))
            )::int
        )                                                 AS req_count
    FROM hours
),
expanded AS (
    -- Materialize req_count rows per hour. Each row = one upstream call.
    SELECT
        s.bucket,
        s.rn,
        gs2 AS sub_idx
    FROM shaped s
    CROSS JOIN LATERAL generate_series(1, s.req_count) gs2
)
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
)
SELECT
    '11111111-1111-1111-1111-111111111111'::uuid,
    'seed-' || rn || '-' || sub_idx,
    'router.upstream',
    'trace-seed-' || rn || '-' || sub_idx,
    bucket + (sub_idx * INTERVAL '11 seconds'),
    'claude-opus-4-5',
    -- ~70% routed to a cheaper model, 30% stayed on the requested model.
    CASE WHEN (sub_idx % 10) < 7 THEN 'claude-haiku-4-5' ELSE 'claude-opus-4-5' END,
    'anthropic',
    CASE WHEN (sub_idx % 10) < 7 THEN 'cluster:downgrade' ELSE 'cluster:keep' END,
    1500 + (sub_idx % 7) * 200,
    (sub_idx % 9) = 0,
    1500 + (sub_idx % 7) * 200,
    600 + (sub_idx % 5) * 120,
    -- Requested cost (Opus pricing): ~$15/M input, ~$75/M output.
    ((1500 + (sub_idx % 7) * 200) / 1000000.0) * 15.00,
    ((600  + (sub_idx % 5) * 120) / 1000000.0) * 75.00,
    -- Actual cost: Haiku rates when downgraded ($1/M, $5/M), Opus rates otherwise.
    CASE WHEN (sub_idx % 10) < 7
         THEN ((1500 + (sub_idx % 7) * 200) / 1000000.0) * 1.00
         ELSE ((1500 + (sub_idx % 7) * 200) / 1000000.0) * 15.00
    END,
    CASE WHEN (sub_idx % 10) < 7
         THEN ((600 + (sub_idx % 5) * 120) / 1000000.0) * 5.00
         ELSE ((600 + (sub_idx % 5) * 120) / 1000000.0) * 75.00
    END,
    20 + (sub_idx % 30),
    400 + (sub_idx % 800),
    420 + (sub_idx % 830),
    FALSE,
    200
FROM expanded
ON CONFLICT (installation_id, request_id, span_type) DO NOTHING;

COMMIT;

-- Quick sanity check.
SELECT
    COUNT(*)                                          AS rows,
    MIN(timestamp)                                    AS earliest,
    MAX(timestamp)                                    AS latest,
    ROUND(SUM(requested_input_cost_usd + requested_output_cost_usd)::numeric, 2) AS requested_usd,
    ROUND(SUM(actual_input_cost_usd + actual_output_cost_usd)::numeric, 2)       AS actual_usd
FROM router.model_router_request_telemetry
WHERE installation_id = '11111111-1111-1111-1111-111111111111';
