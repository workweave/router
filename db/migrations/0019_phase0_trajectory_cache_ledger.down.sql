BEGIN;

ALTER TABLE router.model_router_request_telemetry
    DROP COLUMN trajectory_id,
    DROP COLUMN turn_idx,
    DROP COLUMN step_record,
    DROP COLUMN step_record_version,
    DROP COLUMN action_distribution,
    DROP COLUMN eligible_models,
    DROP COLUMN override_reason;

ALTER TABLE router.session_pins
    DROP COLUMN trajectory_id,
    DROP COLUMN parent_trajectory_id,
    DROP COLUMN cache_ledger,
    DROP COLUMN cumulative_spend_microusd,
    DROP COLUMN switch_count;

COMMIT;
