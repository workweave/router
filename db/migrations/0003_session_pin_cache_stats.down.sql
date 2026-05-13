BEGIN;

ALTER TABLE router.session_pins
    DROP COLUMN last_turn_ended_at,
    DROP COLUMN last_output_tokens,
    DROP COLUMN last_cached_write_tokens,
    DROP COLUMN last_cached_read_tokens,
    DROP COLUMN last_input_tokens;

COMMIT;
