BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN candidate_scores,
    DROP COLUMN propensity;

COMMIT;
