BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN routing_alpha;

COMMIT;
