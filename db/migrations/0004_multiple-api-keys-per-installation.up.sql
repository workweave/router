BEGIN;

-- Allow multiple active router API keys per installation. The partial unique
-- index treated keys as the installation's single bearer secret; we now treat
-- them as named credentials the customer can issue, rotate, and revoke
-- individually.
DROP INDEX router.model_router_api_keys_installation_active_unique;

COMMENT ON TABLE router.model_router_api_keys IS
  'Rotatable bearer keys (rk_ prefix). An installation may hold multiple active keys; identity is carried by router.model_router_users.';

COMMIT;
