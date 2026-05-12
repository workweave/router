BEGIN;

-- Squashed initial schema for the router. This collapses the original
-- migrations 0001-0011 into a single fresh-install baseline ahead of
-- open-sourcing the router. The shape below is the final state produced
-- by running 0001..0011 in order, with no intermediate columns or
-- constraints that were later dropped/renamed.
--
-- Existing deployed databases (which already ran 0001..0011) must have
-- their migrate bookkeeping force-set to version 1 *without* re-running
-- this file; the schema is already in place there. New installations
-- run only this file.

CREATE SCHEMA IF NOT EXISTS router;

-- ---------------------------------------------------------------------------
-- Installations: customer router tenants. Owns API keys and is the FK target
-- for every other table in this schema.
-- ---------------------------------------------------------------------------
CREATE TABLE router.model_router_installations (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  external_id     VARCHAR(36) NOT NULL,
  name            VARCHAR(255) NOT NULL,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at      TIMESTAMP,
  created_by      VARCHAR(36)
);

CREATE INDEX model_router_installations_external_id_idx
  ON router.model_router_installations(external_id);

CREATE UNIQUE INDEX model_router_installations_name_external_id_unique
  ON router.model_router_installations(external_id, name)
  WHERE deleted_at IS NULL;

COMMENT ON TABLE router.model_router_installations IS 'Customer router installations; owns API keys';

-- ---------------------------------------------------------------------------
-- API keys: rotatable bearer tokens (rk_ prefix) scoped to an installation.
-- Identity is carried by router.model_router_users; a key is an installation-
-- scoped secret, not a per-user credential, hence the one-active-key
-- invariant enforced by the partial unique index below.
-- ---------------------------------------------------------------------------
CREATE TABLE router.model_router_api_keys (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  external_id     VARCHAR(36) NOT NULL,
  name            VARCHAR(255),
  key_prefix      VARCHAR(16) NOT NULL,
  key_hash        VARCHAR(255) NOT NULL,
  key_suffix      VARCHAR(4) NOT NULL,
  last_used_at    TIMESTAMP,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at      TIMESTAMP,
  created_by      VARCHAR(36)
);

CREATE INDEX model_router_api_keys_external_id_idx
  ON router.model_router_api_keys(external_id);

CREATE INDEX model_router_api_keys_installation_id_idx
  ON router.model_router_api_keys(installation_id);

CREATE UNIQUE INDEX model_router_api_keys_key_hash_unique
  ON router.model_router_api_keys(key_hash)
  WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX model_router_api_keys_installation_active_unique
  ON router.model_router_api_keys(installation_id)
  WHERE deleted_at IS NULL;

COMMENT ON TABLE router.model_router_api_keys IS 'Rotatable bearer keys (rk_ prefix) per installation';
COMMENT ON COLUMN router.model_router_api_keys.key_hash IS 'SHA-256 of the full rk_ token';
COMMENT ON COLUMN router.model_router_api_keys.key_suffix IS 'Last 4 chars for display';
COMMENT ON INDEX router.model_router_api_keys_installation_active_unique IS
  'One active key per installation. Identity is carried by router.model_router_users; a key is an installation-scoped secret, not a per-user credential.';

-- ---------------------------------------------------------------------------
-- External (BYOK) provider API keys: customer-owned upstream credentials we
-- route requests through. Ciphertext is AES-256-GCM with the 12-byte nonce
-- prepended.
-- ---------------------------------------------------------------------------
CREATE TABLE router.model_router_external_api_keys (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id    UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  external_id        VARCHAR(36) NOT NULL,
  provider           VARCHAR(32) NOT NULL CHECK (provider IN ('anthropic','openai','google','openrouter','fireworks')),
  key_ciphertext     BYTEA NOT NULL,
  key_prefix         VARCHAR(16) NOT NULL,
  key_suffix         VARCHAR(4) NOT NULL,
  key_fingerprint    VARCHAR(64) NOT NULL,
  name               VARCHAR(255),
  last_used_at       TIMESTAMP,
  created_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at         TIMESTAMP,
  created_by         VARCHAR(36)
);

-- Hot-path lookup: (installation_id) -> all active external keys, filtered
-- at app layer by provider.
CREATE INDEX model_router_external_api_keys_installation_active_idx
  ON router.model_router_external_api_keys (installation_id, created_at DESC)
  WHERE deleted_at IS NULL;

-- One key per (installation, provider) when active.
CREATE UNIQUE INDEX model_router_external_api_keys_installation_provider_active_idx
  ON router.model_router_external_api_keys (installation_id, provider)
  WHERE deleted_at IS NULL;

-- Cross-tenant guard index.
CREATE INDEX model_router_external_api_keys_external_id_idx
  ON router.model_router_external_api_keys (external_id);

COMMENT ON TABLE router.model_router_external_api_keys IS 'Customer-owned provider API keys for BYOK routing';
COMMENT ON COLUMN router.model_router_external_api_keys.key_ciphertext IS 'AES-256-GCM encrypted API key with 12-byte nonce prepended';
COMMENT ON COLUMN router.model_router_external_api_keys.key_fingerprint IS 'SHA-256 of plaintext for deduplication display';

-- ---------------------------------------------------------------------------
-- Session pins: session-sticky routing decisions with a sliding 1h TTL that
-- matches the Anthropic prompt cache window.
-- ---------------------------------------------------------------------------
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

-- ---------------------------------------------------------------------------
-- Request telemetry: one row per span emitted by the router during a
-- request, used for cost attribution, latency analysis, and cluster-router
-- observation.
-- ---------------------------------------------------------------------------
CREATE TABLE router.model_router_request_telemetry (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id           UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    request_id                VARCHAR NOT NULL,
    span_type                 VARCHAR NOT NULL,
    trace_id                  VARCHAR NOT NULL,
    timestamp                 TIMESTAMPTZ NOT NULL,
    requested_model           VARCHAR,
    decision_model            VARCHAR,
    decision_provider         VARCHAR,
    decision_reason           VARCHAR,
    estimated_input_tokens    INT DEFAULT 0,
    sticky_hit                BOOLEAN DEFAULT FALSE,
    embed_input               VARCHAR,
    input_tokens              INT DEFAULT 0,
    output_tokens             INT DEFAULT 0,
    requested_input_cost_usd  NUMERIC(16, 6) DEFAULT 0,
    requested_output_cost_usd NUMERIC(16, 6) DEFAULT 0,
    actual_input_cost_usd     NUMERIC(16, 6) DEFAULT 0,
    actual_output_cost_usd    NUMERIC(16, 6) DEFAULT 0,
    route_latency_ms          BIGINT,
    upstream_latency_ms       BIGINT,
    total_latency_ms          BIGINT,
    cross_format              BOOLEAN DEFAULT FALSE,
    upstream_status_code      INT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    cluster_ids               INT[],
    candidate_models          TEXT[],
    chosen_score              DOUBLE PRECISION,
    alpha_breakdown           JSONB,
    cluster_router_version    VARCHAR,
    ttft_ms                   BIGINT,
    cache_creation_tokens     INT,
    cache_read_tokens         INT,
    device_id                 VARCHAR,
    session_id                VARCHAR
);

CREATE INDEX ON router.model_router_request_telemetry (installation_id, timestamp DESC);
CREATE UNIQUE INDEX ON router.model_router_request_telemetry (installation_id, request_id, span_type);

-- ---------------------------------------------------------------------------
-- Router users: end-user identities seen on inbound requests, scoped to an
-- installation. Either email or claude_account_uuid must be present; the
-- partial unique indexes scope each identity dimension independently so an
-- account-only row and an email-only row don't collide.
-- ---------------------------------------------------------------------------
CREATE TABLE router.model_router_users (
  id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id       UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  email                 TEXT,
  claude_account_uuid   UUID,
  first_seen_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_seen_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at            TIMESTAMP,
  CONSTRAINT model_router_users_identity_present
    CHECK (email IS NOT NULL OR claude_account_uuid IS NOT NULL)
);

CREATE INDEX model_router_users_installation_id_idx
  ON router.model_router_users(installation_id);

CREATE UNIQUE INDEX model_router_users_installation_email_unique
  ON router.model_router_users(installation_id, email)
  WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX model_router_users_installation_account_unique
  ON router.model_router_users(installation_id, claude_account_uuid)
  WHERE email IS NULL
    AND claude_account_uuid IS NOT NULL
    AND deleted_at IS NULL;

COMMENT ON TABLE router.model_router_users IS 'End-user identities seen on inbound requests, scoped to an installation. Replaces the per-user API key pattern.';
COMMENT ON COLUMN router.model_router_users.email IS
  'Lowercased, trimmed user email (typically git user.email). Nullable: Claude CLI versions that send only account_uuid in metadata.user_id produce email-NULL rows keyed on (installation_id, claude_account_uuid).';
COMMENT ON COLUMN router.model_router_users.claude_account_uuid IS 'Optional Claude Code account_uuid carried in metadata.user_id; informational only.';

COMMIT;
