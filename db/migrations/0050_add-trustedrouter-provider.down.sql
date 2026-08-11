BEGIN;

-- Rows must go before the constraint is narrowed, or the ADD CONSTRAINT fails
-- validation against existing TrustedRouter BYOK keys.
DELETE FROM router.model_router_external_api_keys
WHERE provider = 'trustedrouter';

-- Restores the 0045 allowlist exactly.
ALTER TABLE router.model_router_external_api_keys
  DROP CONSTRAINT model_router_external_api_keys_provider_check;

ALTER TABLE router.model_router_external_api_keys
  ADD CONSTRAINT model_router_external_api_keys_provider_check
  CHECK (provider IN (
    'anthropic','openai','google','openrouter','fireworks',
    'bedrock','makora','together','xai','anthropic_gateway'
  ));

COMMIT;
