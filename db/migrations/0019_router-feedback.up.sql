BEGIN;

-- One row per /router-feedback submission (see
-- internal/proxy/router_feedback.go). Free-form user feedback about a routing
-- decision or model performance, joined to the session via session_key and to
-- request telemetry via installation_id + created_at. served_model is the
-- model the session pin last served when the feedback arrived — the model the
-- user is most likely complaining about.
CREATE TABLE router.router_feedback (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installation_id UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    session_key     BYTEA NOT NULL,
    role            VARCHAR NOT NULL,
    router_user_id  UUID,
    client_app      TEXT,
    session_id      VARCHAR,
    requested_model VARCHAR NOT NULL,
    served_model    VARCHAR NOT NULL,
    feedback        TEXT NOT NULL
);

-- Dashboard / triage queries: recent feedback per installation.
CREATE INDEX router_feedback_installation_id_created_at_idx
    ON router.router_feedback (installation_id, created_at DESC);

-- Session join against router.session_pins / loop_escalation_events.
CREATE INDEX router_feedback_session_key_idx
    ON router.router_feedback (session_key);

COMMENT ON TABLE router.router_feedback IS 'User-submitted /router-feedback about routing decisions or model performance';
COMMENT ON COLUMN router.router_feedback.session_key IS '16-byte digest matching router.session_pins.session_key';

COMMIT;
