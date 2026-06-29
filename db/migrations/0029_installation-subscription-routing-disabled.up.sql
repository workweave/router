BEGIN;

-- Per-installation toggle to disable subscription-AWARE ROUTING for this org.
-- When true, the scorer's subscription subsidy bonus is suppressed, so routing
-- decides purely on quality/cost/speed merits and non-Claude models (GLM, Kimi,
-- DeepSeek, ...) compete fairly instead of always losing to the subsidized
-- Claude family. This removes only the routing BIAS: a turn that still routes to
-- Claude on its own merits is dispatched on the caller's subscription token
-- exactly as before, so the prepaid billing path is unchanged. Default false
-- preserves today's behavior for every existing installation.
ALTER TABLE router.model_router_installations
  ADD COLUMN subscription_routing_disabled BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
