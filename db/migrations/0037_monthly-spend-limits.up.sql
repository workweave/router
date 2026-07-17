BEGIN;

-- Org-scoped spend-limit configuration. One row per organization, written by
-- the Weave control plane (like organization_autopay_config); the router only
-- reads it on the request path. NULL means "no limit of that kind":
--   user_monthly_limit_usd_micros -- default monthly cap applied to every
--       engineer (router user) in the org, overridable per user below.
--   org_monthly_limit_usd_micros  -- hard cap on the org's total inference
--       spend per UTC calendar month, independent of prepaid balance.
CREATE TABLE router.organization_spend_limits (
    organization_id               VARCHAR(36) PRIMARY KEY,
    user_monthly_limit_usd_micros BIGINT,
    org_monthly_limit_usd_micros  BIGINT,
    created_by                    VARCHAR(36),
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Per-engineer override of the org-wide default monthly limit. A row exists
-- only when an admin sets an override; NULL monthly_limit_usd_micros means
-- "explicitly uncapped" (beats the org default), no row means "use the org
-- default".
CREATE TABLE router.model_router_user_spend_limits (
    router_user_id           UUID PRIMARY KEY
        REFERENCES router.model_router_users (id) ON DELETE CASCADE,
    monthly_limit_usd_micros BIGINT,
    created_by               VARCHAR(36),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Month-bucketed spend counters, bumped in the same transaction as the org
-- debit (mirrors model_router_api_keys.spent_usd_micros) so limit enforcement
-- reads a single indexed row instead of summing the ledger. month is the
-- first day of the UTC calendar month.
CREATE TABLE router.model_router_user_monthly_spend (
    router_user_id   UUID NOT NULL
        REFERENCES router.model_router_users (id) ON DELETE CASCADE,
    month            DATE NOT NULL,
    spent_usd_micros BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (router_user_id, month)
);

CREATE TABLE router.organization_monthly_spend (
    organization_id  VARCHAR(36) NOT NULL,
    month            DATE NOT NULL,
    spent_usd_micros BIGINT NOT NULL DEFAULT 0,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (organization_id, month)
);

COMMIT;
