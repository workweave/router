BEGIN;

-- Per-installation model priority list (the "preferred models" ranking). An
-- ordered set of model IDs the org wants the router to lean toward: array
-- position is the rank (index 0 = first preference). The scorer turns each
-- entry into a small additive, rank-decaying score bonus (a soft "finger on the
-- scale") layered on top of the quality/cost blend -- it tilts close calls
-- toward a preferred model but never overrides a clearly-better model for the
-- task. Empty array means "no preference" -- the scorer routes on its merits.
ALTER TABLE router.model_router_installations
  ADD COLUMN preferred_models TEXT[] NOT NULL DEFAULT '{}';

COMMIT;
