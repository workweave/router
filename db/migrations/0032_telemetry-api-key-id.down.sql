BEGIN;

DROP INDEX router.idx_router_request_telemetry_api_key_id;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN api_key_id;

COMMIT;
