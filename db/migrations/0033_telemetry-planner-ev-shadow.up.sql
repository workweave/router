BEGIN;

-- Persist the cache-aware planner's per-turn EV verdict so switch-policy
-- counterfactuals are replayable offline against billed cost. Pure telemetry;
-- all nullable — NULL when the planner did not run.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN planner_outcome              VARCHAR,
    ADD COLUMN planner_reason               VARCHAR,
    ADD COLUMN planner_expected_savings_usd DOUBLE PRECISION,
    ADD COLUMN planner_eviction_cost_usd    DOUBLE PRECISION,
    ADD COLUMN planner_threshold_usd        DOUBLE PRECISION,
    ADD COLUMN planner_pin_cache_cold       BOOLEAN,
    ADD COLUMN planner_pin_model            VARCHAR;

COMMENT ON COLUMN router.model_router_request_telemetry.planner_outcome IS
    'Planner verdict for this turn: stay or switch. NULL when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_reason IS
    'Planner reason (ev_positive, ev_negative, tier_upgrade, no_pin, ...). NULL when the planner did not run.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_expected_savings_usd IS
    'Expected switch savings over the planner horizon in USD (can be negative). NULL unless the EV math ran.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_eviction_cost_usd IS
    'One-time cost of abandoning the pin''s warm prompt cache in USD. NULL unless the EV math ran.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_threshold_usd IS
    'The switch EV threshold the verdict was compared against. NULL unless the EV math ran.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_pin_cache_cold IS
    'Whether the EV math priced the pin''s upstream prompt cache as cold. NULL unless the EV math ran.';
COMMENT ON COLUMN router.model_router_request_telemetry.planner_pin_model IS
    'The pinned (from) model the planner weighed against the fresh recommendation. NULL on no_pin rows and when the planner did not run.';

-- Recreate the production-traffic view so the new columns surface through it
-- (SELECT * freezes the column list at creation). The refreshed freeze also
-- picks up api_key_id (added by 0031 without a view refresh).
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
