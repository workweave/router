BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN preferred_models;

COMMIT;
