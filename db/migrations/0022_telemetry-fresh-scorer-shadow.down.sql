BEGIN;

-- Rebuild the view against the pre-0022 column set first (its frozen SELECT *
-- list still references the dropped columns otherwise).
DROP VIEW router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN pin_age_sec,
    DROP COLUMN fresh_candidate_scores,
    DROP COLUMN fresh_decision_model;

CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
