BEGIN;

-- Rebuild the view against the pre-0021 column set first (it would otherwise
-- still reference the dropped columns through its frozen SELECT * list).
DROP VIEW router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN role,
    DROP COLUMN session_key;

CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
