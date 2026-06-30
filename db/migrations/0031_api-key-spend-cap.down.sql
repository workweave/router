BEGIN;

DROP INDEX router.idx_router_request_telemetry_api_key_id;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN api_key_id;

ALTER TABLE router.model_router_api_keys
    DROP COLUMN spent_usd_micros,
    DROP COLUMN spend_cap_usd_micros;

COMMIT;
