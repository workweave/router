BEGIN;

ALTER TABLE router.session_pins
    ADD COLUMN last_input_tokens         INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_cached_read_tokens   INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_cached_write_tokens  INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_output_tokens        INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN last_turn_ended_at        TIMESTAMPTZ;

COMMIT;
