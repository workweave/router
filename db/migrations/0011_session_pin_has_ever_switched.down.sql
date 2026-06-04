BEGIN;

ALTER TABLE router.session_pins
  DROP COLUMN has_ever_switched;

COMMIT;
