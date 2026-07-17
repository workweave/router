BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN usage_details,
    DROP COLUMN usage_authority_status;

COMMIT;
