BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN subscription_routing_disabled;

COMMIT;
