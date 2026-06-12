BEGIN;

-- Phase 0 of the learned-routing-policy plan (docs/new_architecture/PLAN.md):
-- the trajectory + cache-ledger substrate every later phase trains on.

-- ---------------------------------------------------------------------------
-- session_pins: cache ledger + session aggregates + stable trajectory id.
--
-- trajectory_id is minted once per pin row and survives session-key drift
-- (the derived key can shift when compaction rewrites the first user
-- message); telemetry rows copy it so trajectories are joinable offline.
--
-- cache_ledger is an LRU map over the last few (provider, model) pairs the
-- session has been served by. Per entry: last-turn timestamp, last usage
-- token counts, consecutive turns, and the last warm-prefix reconciliation
-- error. It generalizes the single-slot last_* columns (which stay for the
-- planner) so a policy can see that switching BACK to a recently-used model
-- within its provider's cache TTL is nearly free.
-- ---------------------------------------------------------------------------
ALTER TABLE router.session_pins
    ADD COLUMN trajectory_id             UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN parent_trajectory_id      UUID,
    ADD COLUMN cache_ledger              JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN cumulative_spend_microusd BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN switch_count              INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN router.session_pins.trajectory_id IS
  'Stable trajectory UUID minted on first pin; survives session-key drift, copied onto telemetry rows';
COMMENT ON COLUMN router.session_pins.parent_trajectory_id IS
  'Trajectory of the dispatching parent session for sub-agent threads; NULL for main loops';
COMMENT ON COLUMN router.session_pins.cache_ledger IS
  'LRU map keyed "provider/model" -> {last_turn_at, last_input_tokens, last_cache_read_tokens, last_cache_creation_tokens, last_output_tokens, consecutive_turns, last_reconcile_error_tokens}';
COMMENT ON COLUMN router.session_pins.cumulative_spend_microusd IS
  'Running actual upstream spend for the session in micro-USD, accumulated on usage writeback';
COMMENT ON COLUMN router.session_pins.switch_count IS
  'Number of served-model switches observed on usage writeback (complements has_ever_switched latch)';

-- ---------------------------------------------------------------------------
-- request telemetry: the logged observation/action record the trainer
-- consumes verbatim (log-and-train-on-logged; PLAN.md G10). step_record is
-- the versioned fixed-shape observation the policy saw; action_distribution
-- is the full behavior-policy distribution over the eligible set (model ->
-- propensity), not just the chosen action's propensity, so DR can evaluate
-- any target policy later.
-- ---------------------------------------------------------------------------
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN trajectory_id        UUID,
    ADD COLUMN turn_idx             INT,
    ADD COLUMN step_record          JSONB,
    ADD COLUMN step_record_version  INT,
    ADD COLUMN action_distribution  JSONB,
    ADD COLUMN eligible_models      TEXT[],
    ADD COLUMN override_reason      VARCHAR;

COMMENT ON COLUMN router.model_router_request_telemetry.trajectory_id IS
  'Copied from session_pins.trajectory_id at decision time; the offline trajectory join key';
COMMENT ON COLUMN router.model_router_request_telemetry.turn_idx IS
  'Atomic per-trajectory turn counter (pins row), not timestamp ordering';
COMMENT ON COLUMN router.model_router_request_telemetry.step_record IS
  'Versioned observation record computed in the Go hot path; trainers consume it verbatim and never reconstruct features';
COMMENT ON COLUMN router.model_router_request_telemetry.step_record_version IS
  'Schema version of step_record; trainers refuse mixed versions without a migration map';
COMMENT ON COLUMN router.model_router_request_telemetry.action_distribution IS
  'Behavior policy distribution over eligible models {model: probability}; NULL on non-policy spans';
COMMENT ON COLUMN router.model_router_request_telemetry.eligible_models IS
  'Eligible set after provider/exclusion/capability/context filters, at decision time';
COMMENT ON COLUMN router.model_router_request_telemetry.override_reason IS
  'Set when a safety override (hard pin, force-model, error failover) bypassed the policy; row excluded from policy training';


COMMIT;
