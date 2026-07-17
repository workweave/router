BEGIN;

-- Phase 0 of the Claude Code cost-observing proxy design (see
-- docs/internal/claude-code-cost-proxy-design.md in the WorkWeave monorepo):
-- capture the full anthropic-ratelimit-unified-* header set observed on
-- subscription-served /v1/messages turns. The router already parses this
-- family of headers into a Snapshot for the subscription-subsidy feature
-- (internal/proxy/usage), but discards the fields that matter for billing
-- classification (unified-status, overage-status, overage-disabled-reason,
-- representative-claim). This column is instrumentation only — nothing reads
-- it yet — captured verbatim as jsonb so header-shape changes surface in the
-- data rather than silently misclassifying. NULL on non-subscription turns and
-- on rows written before this column existed.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN unified_limit_headers JSONB;

COMMIT;
