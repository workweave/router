BEGIN;

-- Rebuild the view against the pre-0033 column set first (its frozen SELECT *
-- list still references the dropped columns otherwise).
DROP VIEW router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN planner_outcome,
    DROP COLUMN planner_reason,
    DROP COLUMN planner_expected_savings_usd,
    DROP COLUMN planner_eviction_cost_usd,
    DROP COLUMN planner_threshold_usd,
    DROP COLUMN planner_pin_cache_cold,
    DROP COLUMN planner_pin_model;

-- Recreate the view with the explicit pre-0033 column list rather than
-- SELECT *. The production view was last frozen by migration 0028, BEFORE
-- 0031 added api_key_id, so the pre-0033 view does not reference api_key_id.
-- A SELECT * here would freeze api_key_id in and make 0031's down migration
-- fail ("cannot drop column api_key_id ... view depends on it") on a full
-- roll-down.
CREATE VIEW router.production_request_telemetry AS
SELECT
    id,
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
    created_at,
    cluster_ids,
    candidate_models,
    chosen_score,
    alpha_breakdown,
    cluster_router_version,
    ttft_ms,
    cache_creation_tokens,
    cache_read_tokens,
    device_id,
    session_id,
    candidate_scores,
    propensity,
    router_user_id,
    client_app,
    turn_type,
    rollout_id,
    upstream_finish_reason,
    stop_reason,
    tool_use_blocks,
    invalid_tool_args_blocks,
    failover_used,
    degenerate_shadow,
    session_key,
    role,
    fresh_decision_model,
    fresh_candidate_scores,
    pin_age_sec,
    tool_result_bytes,
    credential_key_prefix,
    credential_key_suffix,
    credential_source
FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
