BEGIN;

CREATE TABLE router.session_pins (
  session_key       BYTEA NOT NULL,
  role              VARCHAR(32) NOT NULL DEFAULT 'default',
  installation_id   UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  pinned_provider   VARCHAR(32) NOT NULL,
  pinned_model      VARCHAR(128) NOT NULL,
  decision_reason   TEXT NOT NULL,
  turn_count        INT NOT NULL DEFAULT 1,
  pinned_until      TIMESTAMP NOT NULL,
  first_pinned_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_seen_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (session_key, role)
);

CREATE INDEX session_pins_pinned_until_idx
  ON router.session_pins(pinned_until);

COMMENT ON TABLE router.session_pins IS 'Session-sticky routing pins; sliding 1h TTL matching Anthropic prompt cache';
COMMENT ON COLUMN router.session_pins.session_key IS '16-byte digest derived from api_key_id + (metadata.user_id | system+first-user hashes)';
COMMENT ON COLUMN router.session_pins.role IS 'Stage 1 always emits "default"; turn-type roles land with §3.3';

COMMIT;
