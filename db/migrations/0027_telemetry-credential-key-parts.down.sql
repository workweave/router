BEGIN;

-- Rebuild the view against the pre-0027 column set first (its frozen SELECT *
-- list still references the dropped columns otherwise).
DROP VIEW router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN credential_key_prefix,
    DROP COLUMN credential_key_suffix,
    DROP COLUMN credential_source;

CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
