BEGIN;

-- Per-installation routing preference (the "quality vs price" dial). Stored as
-- the scorer's quality weight (Alpha) -- a normalized fraction in [0, 1] where
-- 1.0 biases routing fully toward quality and 0.0 fully toward price. The
-- implied price weight is the remainder (1 - quality). NULL means "no
-- preference" -- the scorer keeps its tuned per-cluster defaults.
ALTER TABLE router.model_router_installations
  ADD COLUMN routing_quality_weight DOUBLE PRECISION;

COMMIT;
