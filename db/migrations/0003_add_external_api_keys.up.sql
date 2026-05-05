BEGIN;

CREATE TABLE router.model_router_external_api_keys (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id    UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  external_id        VARCHAR(36) NOT NULL,
  provider           VARCHAR(32) NOT NULL CHECK (provider IN ('anthropic','openai','google')),
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

-- Hot-path lookup: (installation_id) -> all active external keys
-- Filtered at app layer by provider
CREATE INDEX model_router_external_api_keys_installation_active_idx
  ON router.model_router_external_api_keys (installation_id, created_at DESC)
  WHERE deleted_at IS NULL;

-- One key per (installation, provider) when active
CREATE UNIQUE INDEX model_router_external_api_keys_installation_provider_active_idx
  ON router.model_router_external_api_keys (installation_id, provider)
  WHERE deleted_at IS NULL;

-- Cross-tenant guard index
CREATE INDEX model_router_external_api_keys_external_id_idx
  ON router.model_router_external_api_keys (external_id);

COMMENT ON TABLE router.model_router_external_api_keys IS 'Customer-owned provider API keys for BYOK routing';
COMMENT ON COLUMN router.model_router_external_api_keys.key_ciphertext IS 'AES-256-GCM encrypted API key with 12-byte nonce prepended';
COMMENT ON COLUMN router.model_router_external_api_keys.key_fingerprint IS 'SHA-256 of plaintext for deduplication display';

COMMIT;
