BEGIN;

ALTER TABLE router.session_pins
    DROP COLUMN last_served_model;

COMMIT;
