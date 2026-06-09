BEGIN;

-- Off-policy logging substrate (additive, nullable). candidate_scores is the
-- full pre-argmax model->score map; propensity is the probability the chosen
-- model was selected under the acting policy (1.0 for deterministic argmax,
-- <1.0 only when an exploration policy randomizes). Both are NULL for
-- non-cluster decisions and pinned-route turns, exactly like the existing
-- routing-brain columns.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN candidate_scores JSONB,
    ADD COLUMN propensity       DOUBLE PRECISION;

COMMIT;
