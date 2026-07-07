BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN strategy,
    DROP COLUMN route_id;

COMMIT;
