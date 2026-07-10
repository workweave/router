BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN policy_route_key,
    DROP COLUMN policy_artifact_id,
    DROP COLUMN policy_artifact_sha256,
    DROP COLUMN roster_version,
    DROP COLUMN sidecar_schema_version,
    DROP COLUMN training_allowed,
    DROP COLUMN capture_mode,
    DROP COLUMN debug_ref;

COMMIT;
