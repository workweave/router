BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN routing_quality_weight,
  DROP COLUMN routing_speed_weight;

COMMIT;
