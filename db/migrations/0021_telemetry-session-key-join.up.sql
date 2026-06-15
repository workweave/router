BEGIN;

-- Add the session join key so request telemetry can be correlated offline with
-- the behavioral-signal sidecar in router.spiral_shadow_events. That table's
-- own header documents the intended join "by session_key against
-- model_router_request_telemetry", but the column was never wired in here, so
-- the join was impossible. (session_key, role) together match the
-- spiral_shadow_events / session_pins composite key exactly.
--
-- session_key is the same 16-byte one-way digest already persisted as BYTEA in
-- session_pins and spiral_shadow_events -- no new identifier is exposed. Both
-- columns are nullable: pre-existing rows stay NULL.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN session_key BYTEA,
    ADD COLUMN role        VARCHAR;

COMMENT ON COLUMN router.model_router_request_telemetry.session_key IS
    '16-byte session digest (matches session_pins / spiral_shadow_events). NULL on rows written before this column existed. Join key to spiral_shadow_events on (installation_id, session_key, role).';
COMMENT ON COLUMN router.model_router_request_telemetry.role IS
    'Session-pin role used for the turn (roleForTier of the requested model). Pairs with session_key to identify the turn thread; matches spiral_shadow_events.role.';

-- Recreate the production-traffic view: a CREATE VIEW ... SELECT * freezes its
-- column list at creation time, so the two new columns would never surface
-- through the view without this rebuild. Definition otherwise unchanged from
-- migration 0019.
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
