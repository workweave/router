BEGIN;

ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN router_user_id UUID,
    ADD COLUMN client_app TEXT;

COMMIT;
