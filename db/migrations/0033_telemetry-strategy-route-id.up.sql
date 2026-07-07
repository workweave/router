BEGIN;

-- Record the routing model (strategy) that produced each decision and the
-- sidecar correlation id, so every telemetry row is labeled by which router
-- chose the model. `strategy` is one of cluster / hmm / rl / bandit and is
-- always populated going forward; `route_id` is the opaque HMM/RL correlation
-- id (NULL for the default cluster scorer). Both are NULL for pre-migration rows.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN strategy VARCHAR,
    ADD COLUMN route_id VARCHAR;

COMMIT;
