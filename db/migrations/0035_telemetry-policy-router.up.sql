BEGIN;

-- Persist the strategy-neutral policy contract fields needed to correlate a
-- served decision with its immutable artifact, candidate roster, and learning
-- eligibility snapshot. Columns remain nullable so pre-migration rows are
-- distinguishable from new requests where training_allowed is explicitly
-- false and capture_mode is explicitly off.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN policy_route_key VARCHAR,
    ADD COLUMN policy_artifact_id VARCHAR,
    ADD COLUMN policy_artifact_sha256 VARCHAR,
    ADD COLUMN roster_version VARCHAR,
    ADD COLUMN sidecar_schema_version VARCHAR,
    ADD COLUMN training_allowed BOOLEAN,
    ADD COLUMN capture_mode VARCHAR,
    ADD COLUMN debug_ref VARCHAR;

COMMIT;
