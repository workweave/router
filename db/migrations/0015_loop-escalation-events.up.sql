BEGIN;

-- Durable record of every cyclic-loop detection (see
-- internal/proxy/loop_detection.go and docs/plans/SPEC_loop_detect_escalate.md).
-- One row per (session, role) detection — the once-per-session budget is
-- enforced by counting rows here, so the detector cannot re-fire after the
-- session pin expires. This is both the ops signal (fire rate, opus-share,
-- rescue rate via session join against model_router_request_telemetry) and the
-- training corpus: (session, looping_model) → looped is the exact misroute the
-- embedder cannot predict up front.
CREATE TABLE router.loop_escalation_events (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installation_id   UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    session_key       BYTEA NOT NULL,
    role              VARCHAR NOT NULL,
    looping_model     VARCHAR NOT NULL,
    -- What the router did about it: 'escalated' (pinned to the escalation
    -- target), 'holdout' (log-not-act measurement bucket), 'already_strong'
    -- (looping model is the escalation target — record-only training signal),
    -- 'user_forced' (a /force-model pin outranks auto-escalation), or
    -- 'disabled' (ROUTER_LOOP_ESCALATION_ENABLED kill switch off).
    action            VARCHAR NOT NULL,
    escalation_target VARCHAR NOT NULL,
    loop_tool         VARCHAR NOT NULL,
    loop_input_hash   VARCHAR NOT NULL,
    repeat_count      INT NOT NULL,
    distinct_ratio    DOUBLE PRECISION NOT NULL,
    window_size       INT NOT NULL
);

-- Budget lookup: has this session already fired?
CREATE INDEX loop_escalation_events_session_key_role_idx
    ON router.loop_escalation_events (session_key, role);

-- Fire-rate / dashboard queries.
CREATE INDEX loop_escalation_events_installation_id_created_at_idx
    ON router.loop_escalation_events (installation_id, created_at DESC);

COMMENT ON TABLE router.loop_escalation_events IS 'Cyclic tool-call loop detections: ops signal and (session, looping_model) -> looped training labels';
COMMENT ON COLUMN router.loop_escalation_events.session_key IS '16-byte digest matching router.session_pins.session_key; join key for post-escalation outcome';

COMMIT;
