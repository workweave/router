BEGIN;

-- Per-installation routing preference ("speed / price / quality" dials).
-- quality maps to the scorer's per-cluster Alpha (uniformly overridden) and
-- speed maps to SpeedWeight; price is the implied remainder (1 - quality -
-- speed). Both NULL means "no preference" -- the scorer keeps its tuned
-- per-cluster defaults. Stored as normalized fractions in [0, 1].
ALTER TABLE router.model_router_installations
  ADD COLUMN routing_quality_weight DOUBLE PRECISION,
  ADD COLUMN routing_speed_weight DOUBLE PRECISION;

COMMIT;
