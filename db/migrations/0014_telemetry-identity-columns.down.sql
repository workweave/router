BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN router_user_id,
    DROP COLUMN client_app;

COMMIT;
