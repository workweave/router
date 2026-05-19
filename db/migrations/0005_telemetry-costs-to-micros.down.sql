BEGIN;

ALTER TABLE router.model_router_request_telemetry
    ALTER COLUMN requested_input_cost_usd  DROP DEFAULT,
    ALTER COLUMN requested_output_cost_usd DROP DEFAULT,
    ALTER COLUMN actual_input_cost_usd     DROP DEFAULT,
    ALTER COLUMN actual_output_cost_usd    DROP DEFAULT,
    ALTER COLUMN requested_input_cost_usd  TYPE NUMERIC(16, 6) USING (requested_input_cost_usd  / 1000000.0)::NUMERIC(16, 6),
    ALTER COLUMN requested_output_cost_usd TYPE NUMERIC(16, 6) USING (requested_output_cost_usd / 1000000.0)::NUMERIC(16, 6),
    ALTER COLUMN actual_input_cost_usd     TYPE NUMERIC(16, 6) USING (actual_input_cost_usd     / 1000000.0)::NUMERIC(16, 6),
    ALTER COLUMN actual_output_cost_usd    TYPE NUMERIC(16, 6) USING (actual_output_cost_usd    / 1000000.0)::NUMERIC(16, 6),
    ALTER COLUMN requested_input_cost_usd  SET DEFAULT 0,
    ALTER COLUMN requested_output_cost_usd SET DEFAULT 0,
    ALTER COLUMN actual_input_cost_usd     SET DEFAULT 0,
    ALTER COLUMN actual_output_cost_usd    SET DEFAULT 0;

COMMIT;
