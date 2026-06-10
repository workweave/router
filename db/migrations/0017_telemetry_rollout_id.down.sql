BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN rollout_id;

COMMIT;
