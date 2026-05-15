BEGIN;

CREATE UNIQUE INDEX model_router_api_keys_installation_active_unique
  ON router.model_router_api_keys(installation_id)
  WHERE deleted_at IS NULL;

COMMENT ON INDEX router.model_router_api_keys_installation_active_unique IS
  'One active key per installation. Identity is carried by router.model_router_users; a key is an installation-scoped secret, not a per-user credential.';

COMMENT ON TABLE router.model_router_api_keys IS 'Rotatable bearer keys (rk_ prefix) per installation';

COMMIT;
