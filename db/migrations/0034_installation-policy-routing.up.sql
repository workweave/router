BEGIN;

ALTER TABLE router.model_router_installations
  ADD COLUMN routing_strategy VARCHAR(64),
  ADD COLUMN routing_rollout_id VARCHAR(128),
  ADD COLUMN policy_shadow_strategy VARCHAR(64),
  ADD COLUMN policy_debug_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN policy_header_overrides_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN policy_routing_intent VARCHAR(32),
  ADD COLUMN ai_training_allowed BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN router.model_router_installations.routing_strategy IS
  'Optional serving-strategy override. NULL follows the deployment default.';
COMMENT ON COLUMN router.model_router_installations.ai_training_allowed IS
  'Privacy snapshot synced from the organization AI-training setting. False disables policy learning.';

COMMIT;
