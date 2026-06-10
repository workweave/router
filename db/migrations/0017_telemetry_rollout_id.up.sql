BEGIN;

-- Client-supplied rollout correlation id (x-weave-rollout-id header). Set by
-- eval/training harnesses so a sandbox rollout's graded reward can be joined
-- back onto every routing decision made inside it (bandit closed loop).
-- NULL for all non-harness traffic.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN rollout_id VARCHAR;

-- The reward join scans by rollout id; partial index keeps the hot
-- production path (rollout_id IS NULL) free.
CREATE INDEX model_router_request_telemetry_rollout_id_idx
    ON router.model_router_request_telemetry (rollout_id)
    WHERE rollout_id IS NOT NULL;

COMMIT;
