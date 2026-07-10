BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN ai_training_allowed,
  DROP COLUMN policy_routing_intent,
  DROP COLUMN policy_header_overrides_enabled,
  DROP COLUMN policy_debug_enabled,
  DROP COLUMN policy_shadow_strategy,
  DROP COLUMN routing_rollout_id,
  DROP COLUMN routing_strategy;

COMMIT;
