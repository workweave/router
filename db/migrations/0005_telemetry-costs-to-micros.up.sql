BEGIN;

-- Convert telemetry cost columns from NUMERIC(16,6) USD to BIGINT micros
-- (USD x 1e6). Integer math avoids the rounding-edge surprises pgtype.Numeric
-- introduces at the Go/Postgres boundary and aligns this table with the
-- credit ledger introduced in the billing MVP, which represents money as
-- BIGINT micros.
ALTER TABLE router.model_router_request_telemetry
    ALTER COLUMN requested_input_cost_usd  DROP DEFAULT,
    ALTER COLUMN requested_output_cost_usd DROP DEFAULT,
    ALTER COLUMN actual_input_cost_usd     DROP DEFAULT,
    ALTER COLUMN actual_output_cost_usd    DROP DEFAULT,
    -- COALESCE on the way in: a previous toNumeric bug silently persisted
    -- some historical rows with NULL cost values. Coerce those to 0 micros
    -- so row queries that scan the result into a non-pointer int64 don't
    -- choke on NULL.
    ALTER COLUMN requested_input_cost_usd  TYPE BIGINT USING ROUND(COALESCE(requested_input_cost_usd,  0) * 1000000)::BIGINT,
    ALTER COLUMN requested_output_cost_usd TYPE BIGINT USING ROUND(COALESCE(requested_output_cost_usd, 0) * 1000000)::BIGINT,
    ALTER COLUMN actual_input_cost_usd     TYPE BIGINT USING ROUND(COALESCE(actual_input_cost_usd,     0) * 1000000)::BIGINT,
    ALTER COLUMN actual_output_cost_usd    TYPE BIGINT USING ROUND(COALESCE(actual_output_cost_usd,    0) * 1000000)::BIGINT,
    ALTER COLUMN requested_input_cost_usd  SET DEFAULT 0,
    ALTER COLUMN requested_output_cost_usd SET DEFAULT 0,
    ALTER COLUMN actual_input_cost_usd     SET DEFAULT 0,
    ALTER COLUMN actual_output_cost_usd    SET DEFAULT 0;

COMMIT;
