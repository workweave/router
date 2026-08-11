BEGIN;

-- The provider CHECK is an allowlist, so a TrustedRouter BYOK token is rejected
-- at INSERT without this. Carries forward the full 0045 allowlist.
ALTER TABLE router.model_router_external_api_keys
  DROP CONSTRAINT model_router_external_api_keys_provider_check;

ALTER TABLE router.model_router_external_api_keys
  ADD CONSTRAINT model_router_external_api_keys_provider_check
  CHECK (provider IN (
    'anthropic','openai','google','openrouter','fireworks',
    'bedrock','makora','together','xai','anthropic_gateway',
    'trustedrouter'
  ));

COMMIT;
