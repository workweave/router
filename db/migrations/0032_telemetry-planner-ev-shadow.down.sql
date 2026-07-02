BEGIN;

-- Rebuild the view against the pre-0032 column set first (its frozen SELECT *
-- list still references the dropped columns otherwise).
DROP VIEW router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN planner_outcome,
    DROP COLUMN planner_reason,
    DROP COLUMN planner_expected_savings_usd,
    DROP COLUMN planner_eviction_cost_usd,
    DROP COLUMN planner_threshold_usd,
    DROP COLUMN planner_pin_cache_cold,
    DROP COLUMN planner_pin_model;

CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
