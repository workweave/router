BEGIN;

-- Per-installation subscription usage-bypass gate. When enabled, requests that
-- present a Claude/Codex subscription credential whose observed rate-limit
-- utilization is still below usage_bypass_threshold are passed straight through
-- to the requested model -- no cluster routing, no model substitution, and no
-- billing debit (the turn is served on the customer's own subscription quota).
-- Once observed utilization crosses the threshold, the gate disengages and the
-- normal routing path (scorer + subscription-aware cost discounting) takes over.
--
-- enabled defaults FALSE: strict opt-in, no behavior change until a customer
-- turns it on. threshold is a normalized fraction in [0, 1] (e.g. 0.80 = "kick
-- the router in once 80% of my plan is used"); NULL is treated as the default
-- threshold at request time so the toggle can be on before a value is chosen.
-- The CHECK guards the gate's core invariant at the storage layer (Go-side
-- validation lands with the dashboard mutation): a threshold outside [0, 1]
-- would make `utilization < threshold` always true, pinning the gate open and
-- suppressing this installation's billing indefinitely.
ALTER TABLE router.model_router_installations
  ADD COLUMN usage_bypass_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN usage_bypass_threshold DOUBLE PRECISION CHECK (usage_bypass_threshold BETWEEN 0 AND 1);

COMMIT;
