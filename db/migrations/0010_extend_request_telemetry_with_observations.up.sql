BEGIN;

ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN cluster_ids            INT[],
    ADD COLUMN candidate_models       TEXT[],
    ADD COLUMN chosen_score           DOUBLE PRECISION,
    ADD COLUMN alpha_breakdown        JSONB,
    ADD COLUMN cluster_router_version VARCHAR,
    ADD COLUMN ttft_ms                BIGINT,
    ADD COLUMN cache_creation_tokens  INT,
    ADD COLUMN cache_read_tokens      INT,
    ADD COLUMN device_id              VARCHAR,
    ADD COLUMN session_id             VARCHAR;

COMMIT;
