BEGIN;

-- Verbatim anthropic-ratelimit-unified-* header set per subscription-served
-- /v1/messages turn (Claude Code cost-observing-proxy Phase 0). The subsidy
-- parser keeps only utilization/reset; billing classification needs the full
-- vocabulary verified against real traffic first, so capture raw jsonb.
-- Instrumentation only — nothing reads it yet. NULL on non-subscription turns.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN unified_limit_headers JSONB;

COMMIT;
