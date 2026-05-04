BEGIN;

CREATE SCHEMA IF NOT EXISTS router;

CREATE TABLE router.model_router_installations (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id VARCHAR(36) NOT NULL,
  name            VARCHAR(255) NOT NULL,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at      TIMESTAMP,
  -- Opaque external identifier; no FK to upstream tables.
  created_by      VARCHAR(36),
  -- Trailing position matches prod, where the column was appended by an
  -- earlier ALTER TABLE ADD COLUMN before this squash. Keeping it last avoids
  -- a sqlc-regen churn and a schema-shape divergence between fresh-from-squash
  -- envs and prod.
  is_eval_allowlisted BOOLEAN NOT NULL DEFAULT false
);

CREATE INDEX model_router_installations_organization_id_idx
  ON router.model_router_installations(organization_id);

CREATE UNIQUE INDEX model_router_installations_name_org_unique
  ON router.model_router_installations(organization_id, name)
  WHERE deleted_at IS NULL;

CREATE TABLE router.model_router_api_keys (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  organization_id VARCHAR(36) NOT NULL,
  name            VARCHAR(255),
  key_prefix      VARCHAR(16) NOT NULL,
  key_hash        VARCHAR(255) NOT NULL,
  key_suffix      VARCHAR(4) NOT NULL,
  last_used_at    TIMESTAMP,
  created_at      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at      TIMESTAMP,
  created_by      VARCHAR(36)
);

CREATE INDEX model_router_api_keys_organization_id_idx
  ON router.model_router_api_keys(organization_id);
CREATE INDEX model_router_api_keys_installation_id_idx
  ON router.model_router_api_keys(installation_id);
CREATE UNIQUE INDEX model_router_api_keys_key_hash_unique
  ON router.model_router_api_keys(key_hash)
  WHERE deleted_at IS NULL;

COMMENT ON TABLE router.model_router_installations IS 'Customer router installations; owns API keys';
COMMENT ON COLUMN router.model_router_installations.is_eval_allowlisted IS 'Allows x-weave-disable-cluster header override for eval harness';
COMMENT ON TABLE router.model_router_api_keys IS 'Rotatable bearer keys (rk_ prefix) per installation';
COMMENT ON COLUMN router.model_router_api_keys.key_hash IS 'SHA-256 of the full rk_ token';
COMMENT ON COLUMN router.model_router_api_keys.key_suffix IS 'Last 4 chars for display';

COMMIT;
