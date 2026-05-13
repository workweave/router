BEGIN;

-- Per-installation model exclusion list. When an entry is present, the
-- cluster scorer drops the matching model from the eligible candidate set
-- at request time (sibling to the boot-time provider filter).
-- Empty array means "no exclusion" -- the default.
ALTER TABLE router.model_router_installations
  ADD COLUMN excluded_models TEXT[] NOT NULL DEFAULT '{}';

COMMIT;
