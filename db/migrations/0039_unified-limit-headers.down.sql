BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN unified_limit_headers;

COMMIT;
