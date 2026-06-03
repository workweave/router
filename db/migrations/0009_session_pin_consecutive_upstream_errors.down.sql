BEGIN;

ALTER TABLE router.session_pins DROP COLUMN consecutive_upstream_errors;

COMMIT;
