BEGIN;

-- Durable record of shadow-mode spiral detections (see
-- internal/proxy/spiral_detection.go). One row per (session, role, reason)
-- signal-class crossing — shadow mode takes no routing action; these rows ARE
-- the deliverable. Joined offline by session_key against
-- model_router_request_telemetry / session outcomes to measure fire rate,
-- precision, and lead time on real traffic before any escalation is armed.
CREATE TABLE router.spiral_shadow_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installation_id     UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    session_key         BYTEA NOT NULL,
    role                VARCHAR NOT NULL,
    routed_model        VARCHAR NOT NULL,
    turn_type           VARCHAR NOT NULL,
    -- Which signal class crossed its threshold: 'err_streak' (consecutive
    -- errored tool_results), 'same_file_thrash' (repeat edits to one path),
    -- 'repetition' (fuzzy recent-window action repetition), or 'monologue'
    -- (consecutive assistant turns with no real tool activity).
    reason              VARCHAR NOT NULL,
    -- Full signal snapshot at fire time, recorded regardless of which reason
    -- fired, so thresholds can be re-tuned offline without re-running traffic.
    err_streak          INT NOT NULL,
    errored_results     INT NOT NULL,
    tool_results        INT NOT NULL,
    max_same_file_edits INT NOT NULL,
    -- Truncated sha256 of the most-thrashed file path: confirms "the same
    -- file" across events without persisting customer file names.
    same_file_path_hash VARCHAR NOT NULL,
    repeat_frac         DOUBLE PRECISION NOT NULL,
    monologue_len       INT NOT NULL,
    tool_call_count     INT NOT NULL,
    message_count       INT NOT NULL
);

-- Budget lookup: has this (session, role, reason) already fired?
CREATE INDEX spiral_shadow_events_session_key_role_reason_idx
    ON router.spiral_shadow_events (session_key, role, reason);

-- Fire-rate / dashboard queries.
CREATE INDEX spiral_shadow_events_installation_id_created_at_idx
    ON router.spiral_shadow_events (installation_id, created_at DESC);

COMMENT ON TABLE router.spiral_shadow_events IS 'Shadow-mode spiral (death-march) detections: log-only fire-rate corpus measured on live traffic before escalation is armed';
COMMENT ON COLUMN router.spiral_shadow_events.session_key IS '16-byte digest matching router.session_pins.session_key; join key for session outcome';

COMMIT;
