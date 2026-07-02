BEGIN;

DELETE FROM router.model_router_external_api_keys
WHERE provider = 'trustedrouter';

ALTER TABLE router.model_router_external_api_keys
  DROP CONSTRAINT model_router_external_api_keys_provider_check,
  ADD CONSTRAINT model_router_external_api_keys_provider_check
    CHECK (provider IN ('anthropic','openai','google','openrouter','fireworks'));

COMMIT;
