BEGIN;

-- Per-installation provider exclusion list. When an entry is present, the
-- provider is removed from the request's enabled-provider set, so the
-- scorer, session pins, tier clamp, and dispatch fallback all skip it
-- (sibling to excluded_models, which drops individual models).
-- Empty array means "no exclusion" -- the default.
ALTER TABLE router.model_router_installations
  ADD COLUMN excluded_providers TEXT[] NOT NULL DEFAULT '{}';

COMMIT;
