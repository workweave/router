BEGIN;

-- Shadow-mode instrumentation for the right-sizing study's hysteresis lever
-- (action item #2). On a main_loop turn the cluster scorer always runs and
-- produces a fresh recommendation + score vector, but on a STAY outcome the
-- final decision rehydrates from the session pin (which carries no scores), so
-- buildObservationContext logged NULL candidate_scores. The fresh vector was
-- computed and discarded -- exactly the turns where "we stayed on opus but a
-- cheaper model was within tau" lives.
--
-- These columns capture the fresh scorer's recommendation on EVERY scored turn
-- (including STAY), so the downgrade opportunity can be measured offline (sweep
-- tau over fresh_candidate_scores vs decision_model + catalog tier) before any
-- hysteresis downgrade is armed. Pure telemetry: no routing behavior changes.
-- All nullable: hard-pin / tool_result / non-cluster turns leave them NULL.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN fresh_decision_model   VARCHAR,
    ADD COLUMN fresh_candidate_scores JSONB,
    ADD COLUMN pin_age_sec            BIGINT;

COMMENT ON COLUMN router.model_router_request_telemetry.fresh_decision_model IS
    'The cluster scorer''s fresh pick for this turn, recorded even when the planner returned STAY (decision_model then names the pinned model served). NULL when the scorer did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.fresh_candidate_scores IS
    'The fresh pre-argmax score vector (model -> blended score) from this turn''s scorer run, recorded even on STAY. Sweep tau against served decision_model + catalog tier to measure the hysteresis downgrade opportunity. NULL when the scorer did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.pin_age_sec IS
    'Age of the loaded session pin in seconds at decision time; supports min-dwell analysis for the hysteresis policy. NULL when no pin was loaded.';

-- Recreate the production-traffic view so the new columns surface through it
-- (CREATE VIEW ... SELECT * freezes its column list at creation). Body
-- unchanged from migration 0019/0021.
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
