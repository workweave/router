-- Reads the active pin for a (session_key, role) pair. Single-row;
-- returns sql.ErrNoRows when no pin is recorded yet. The caller checks
-- pinned_until against now() to discard expired rows that the hourly
-- sweep hasn't collected yet.
-- name: GetSessionPin :one
SELECT *
FROM router.session_pins
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar;

-- Upserts a pin, refreshing pinned_until on every hit (sliding TTL).
-- turn_count increments on conflict so we can observe how many turns a
-- single (session_key, role) lives for. installation_id is set on first
-- insert and not touched on update — re-binding a session to a different
-- installation would indicate a bug, not a legitimate state.
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
  last_seen_at    = CURRENT_TIMESTAMP;

-- Garbage-collects pins that have been expired for >24h. The 24h grace
-- means a transient Postgres outage doesn't immediately prune live pins;
-- the hourly sweep is bounded because the row count is one per active
-- session.
-- name: SweepExpiredSessionPins :exec
DELETE FROM router.session_pins
WHERE pinned_until < now() - interval '24 hours';
