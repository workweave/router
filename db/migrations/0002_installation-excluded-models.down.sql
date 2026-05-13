BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN excluded_models;

COMMIT;
