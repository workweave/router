BEGIN;

-- Attribute each request to the api key that authenticated it, so per-key
-- spend can be audited and dashboarded. Nullable: pre-existing rows and any
-- non-keyed path leave it NULL. No FK — organization_id/api keys are joined in
-- the router schema but telemetry deliberately carries no constraints for
-- ingestion throughput.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN api_key_id UUID;

CREATE INDEX idx_router_request_telemetry_api_key_id
    ON router.model_router_request_telemetry (api_key_id, timestamp DESC);

COMMIT;
