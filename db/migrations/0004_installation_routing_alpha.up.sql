BEGIN;

-- Per-installation quality-vs-cost knob. Stored as SMALLINT 0..10 (representing
-- alpha values 0.0..1.0 in 0.1 steps) so the runtime can do exact-equality
-- bundle lookups without float-precision concerns. 5 = 0.5, matching the
-- shipped global default close enough that pre-existing installations don't
-- silently shift toward either pole when the column is backfilled.
ALTER TABLE router.model_router_installations
  ADD COLUMN routing_alpha SMALLINT NOT NULL DEFAULT 5
  CHECK (routing_alpha BETWEEN 0 AND 10);

COMMIT;
