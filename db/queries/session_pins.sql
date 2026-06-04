-- Reads the active pin for a (session_key, role) pair. Single-row;
-- returns sql.ErrNoRows when no pin is recorded yet. The caller checks
-- pinned_until against now() to discard expired rows that the hourly
-- sweep hasn't collected yet. The last_* token columns and
-- last_turn_ended_at carry the previous turn's upstream usage; the
-- planner reads them to weigh switch EV against eviction cost.
-- name: GetSessionPin :one
SELECT *
FROM router.session_pins
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar;

-- Upserts a pin, refreshing pinned_until on every hit (sliding TTL).
-- turn_count increments on conflict so we can observe how many turns a
-- single (session_key, role) lives for. installation_id is set on first
-- insert and not touched on update — re-binding a session to a different
-- installation would indicate a bug, not a legitimate state. The
-- last_*_tokens / last_turn_ended_at columns are deliberately omitted
-- from the ON CONFLICT update set: only UpdateSessionPinUsage writes
-- them, so the at-start-of-turn refresh here cannot clobber the
-- previous turn's usage with zeros before the planner reads it.
--
-- consecutive_upstream_errors is preserved on a same-model refresh (so
-- the two-strike eviction counter accumulates across turns of the same
-- sticky pin) but reset to 0 on a switch (different model = clean
-- slate). The reset on switch also covers the loop-break / force-model
-- pin-expiry writes, which set pinned_model to the empty string.
-- name: UpsertSessionPin :exec
INSERT INTO router.session_pins (
  session_key, role, installation_id, pinned_provider,
  pinned_model, decision_reason, turn_count, pinned_until
) VALUES (
  @session_key::bytea, @role::varchar, @installation_id::uuid,
  @pinned_provider::varchar, @pinned_model::varchar,
  @decision_reason::text, @turn_count::int, @pinned_until::timestamp
)
ON CONFLICT (session_key, role) DO UPDATE SET
  pinned_provider = EXCLUDED.pinned_provider,
  pinned_model    = EXCLUDED.pinned_model,
  decision_reason = EXCLUDED.decision_reason,
  turn_count      = router.session_pins.turn_count + 1,
  pinned_until    = EXCLUDED.pinned_until,
  last_seen_at    = CURRENT_TIMESTAMP,
  consecutive_upstream_errors = CASE
    WHEN router.session_pins.pinned_model = EXCLUDED.pinned_model
      THEN router.session_pins.consecutive_upstream_errors
    ELSE 0
  END;

-- Records the previous turn's upstream token usage on an existing pin
-- row. Fired off the request path after the upstream response
-- completes; the planner reads these columns at the start of the next
-- turn to compute switch EV against eviction cost. The UPDATE matches
-- by (session_key, role); if the pin has been evicted or never
-- existed, zero rows are affected and the adapter wraps that as a
-- successful no-op. last_served_model records the model that actually
-- served this turn; it lives here (not in UpsertSessionPin) so a
-- /force-model upsert cannot overwrite the genuinely-last-served model
-- before the next turn reads it to detect a mid-session model switch.
-- has_ever_switched latches true the first time the just-served model
-- differs from a prior non-empty last_served_model. ModelSwitched (derived
-- from last_served_model) strips stale Anthropic thinking-block signatures
-- only on the single transition turn, but Claude Code re-sends its full
-- transcript every turn, so once a session has switched at all, the latch
-- keeps the emit path stripping on every subsequent same-model turn for the
-- session's life — the only window in which those poisoned blocks would
-- otherwise reach Anthropic and 400.
-- name: UpdateSessionPinUsage :exec
UPDATE router.session_pins
SET last_input_tokens        = @last_input_tokens::int,
    last_cached_read_tokens  = @last_cached_read_tokens::int,
    last_cached_write_tokens = @last_cached_write_tokens::int,
    last_output_tokens       = @last_output_tokens::int,
    last_turn_ended_at       = @last_turn_ended_at::timestamptz,
    has_ever_switched        = has_ever_switched
      OR (last_served_model <> '' AND last_served_model <> @last_served_model::varchar),
    last_served_model        = @last_served_model::varchar
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar;

-- Atomically increments consecutive_upstream_errors and returns the
-- new value. The turn loop calls this after a non-retryable upstream
-- 4xx on a sticky-pinned turn; the returned count drives the
-- two-strike eviction decision. Returns sql.ErrNoRows if no pin
-- exists, which the adapter maps to a no-op (pin must already be
-- evicted by another path, e.g. force-model / loop-break).
-- name: IncrementSessionPinUpstreamErrors :one
UPDATE router.session_pins
SET consecutive_upstream_errors = consecutive_upstream_errors + 1
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar
RETURNING consecutive_upstream_errors;

-- Clears the two-strike counter after a successful turn. UPDATE
-- matches by (session_key, role); zero rows affected on missing pin
-- is a successful no-op like UpdateSessionPinUsage.
-- name: ResetSessionPinUpstreamErrors :exec
UPDATE router.session_pins
SET consecutive_upstream_errors = 0
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar
  AND consecutive_upstream_errors > 0;

-- Garbage-collects pins that have been expired for >24h. The 24h grace
-- means a transient Postgres outage doesn't immediately prune live pins;
-- the hourly sweep is bounded because the row count is one per active
-- session.
-- name: SweepExpiredSessionPins :exec
DELETE FROM router.session_pins
WHERE pinned_until < now() - interval '24 hours';
