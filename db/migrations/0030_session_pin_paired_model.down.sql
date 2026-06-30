BEGIN;

ALTER TABLE router.session_pins
    DROP COLUMN paired_provider,
    DROP COLUMN paired_model;

COMMIT;
