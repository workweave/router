BEGIN;

ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN usage_authority_status VARCHAR,
    ADD COLUMN usage_details JSONB;

COMMENT ON COLUMN router.model_router_request_telemetry.usage_authority_status IS
    'Provider usage authority: authoritative, partial, missing, or contradictory. NULL on rows written before usage authority tracking.';
COMMENT ON COLUMN router.model_router_request_telemetry.usage_details IS
    'Presence-aware canonical token usage and stable contradiction codes. Contains token counts only; never request content.';

COMMIT;
