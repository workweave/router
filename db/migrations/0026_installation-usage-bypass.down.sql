BEGIN;

ALTER TABLE router.model_router_installations
  DROP COLUMN usage_bypass_enabled,
  DROP COLUMN usage_bypass_threshold;

COMMIT;
