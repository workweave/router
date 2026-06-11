BEGIN;

DROP VIEW IF EXISTS router.production_request_telemetry;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN IF EXISTS upstream_finish_reason,
    DROP COLUMN IF EXISTS stop_reason,
    DROP COLUMN IF EXISTS tool_use_blocks,
    DROP COLUMN IF EXISTS invalid_tool_args_blocks,
    DROP COLUMN IF EXISTS failover_used,
    DROP COLUMN IF EXISTS degenerate_shadow;

COMMIT;
