BEGIN;

ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN turn_type VARCHAR;

COMMIT;
