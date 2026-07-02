BEGIN;

-- Shadow corpus for inter-turn switch-policy tuning. The cache-aware planner's
-- EV verdict (STAY vs SWITCH, expected savings, eviction cost, threshold,
-- warmth assumption) was previously visible only on OTel spans, which age out
-- and don't join back to session_pins or billed cost. Persisting it per
-- request makes every STAY/SWITCH counterfactual replayable offline — sweep
-- thresholds, horizons, and cold-pin policies against real billed cost before
-- arming any live policy change. Pure telemetry: no routing behavior changes.
-- All nullable: rows where the planner did not run (hard pins, tool-result
-- stickies, user-forced pins, usage bypass, planner disabled) leave them NULL.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN planner_outcome              VARCHAR,
    ADD COLUMN planner_reason               VARCHAR,
    ADD COLUMN planner_expected_savings_usd DOUBLE PRECISION,
    ADD COLUMN planner_eviction_cost_usd    DOUBLE PRECISION,
    ADD COLUMN planner_threshold_usd        DOUBLE PRECISION,
    ADD COLUMN planner_pin_cache_cold       BOOLEAN,
    ADD COLUMN planner_pin_model            VARCHAR;

COMMENT ON COLUMN router.model_router_request_telemetry.planner_outcome IS
    'The cache-aware planner''s verdict for this turn: stay or switch. NULL when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_reason IS
    'Snake_case planner reason (ev_positive, ev_negative, tier_upgrade, no_pin, ...). NULL when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_expected_savings_usd IS
    'Expected switch savings over the planner horizon in USD (can be negative). Populated only on the EV path; NULL on early-return reasons and when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_eviction_cost_usd IS
    'One-time cost of abandoning the pin''s warm prompt cache in USD. Zero when the pin was priced cold. NULL on early-return reasons and when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_threshold_usd IS
    'The switch EV threshold the verdict was compared against. NULL on early-return reasons and when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_pin_cache_cold IS
    'Whether the EV math priced the pin''s upstream prompt cache as cold (provider cache TTL lapsed). NULL when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_pin_model IS
    'The pinned (from) model the planner weighed against the fresh recommendation — preserved on SWITCH rows where decision_model already names the switched-to model. NULL when the planner did not run.';

-- Recreate the production-traffic view so the new columns surface through it
-- (CREATE VIEW ... SELECT * freezes its column list at creation). Body
-- unchanged from migration 0028.
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
