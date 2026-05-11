BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN session_id,
    DROP COLUMN device_id,
    DROP COLUMN cache_read_tokens,
    DROP COLUMN cache_creation_tokens,
    DROP COLUMN ttft_ms,
    DROP COLUMN cluster_router_version,
    DROP COLUMN alpha_breakdown,
    DROP COLUMN chosen_score,
    DROP COLUMN candidate_models,
    DROP COLUMN cluster_ids;

COMMIT;
