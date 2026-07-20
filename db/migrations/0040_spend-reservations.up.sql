BEGIN;

-- Open spend-cap reservations for reserve-then-settle (#793). One row per
-- in-flight request per applicable scope. Sweeper deletes expired rows and
-- decrements the denormalized reserved_usd_micros counters; settle/release
-- delete by id and only decrement when DELETE … RETURNING yields a row.
CREATE TABLE router.spend_reservations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_kind          VARCHAR(16) NOT NULL
        CHECK (scope_kind IN ('org_month', 'user_month', 'api_key')),
    scope_id            VARCHAR(64) NOT NULL,
    -- First day of the UTC month for *_month scopes; NULL for lifetime api_key.
    month               DATE,
    amount_usd_micros   BIGINT NOT NULL CHECK (amount_usd_micros > 0),
    expires_at          TIMESTAMPTZ NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    router_request_id   VARCHAR(64),
    CONSTRAINT spend_reservations_month_null_for_api_key CHECK (
        (scope_kind = 'api_key' AND month IS NULL)
        OR (scope_kind IN ('org_month', 'user_month') AND month IS NOT NULL)
    )
);

CREATE INDEX spend_reservations_expires_at_idx
    ON router.spend_reservations (expires_at);

CREATE INDEX spend_reservations_scope_idx
    ON router.spend_reservations (scope_kind, scope_id, month);

-- Denormalized in-flight reserved totals so the gate is a single-row
-- spent + reserved + R <= limit check without summing open reservations.
ALTER TABLE router.organization_monthly_spend
    ADD COLUMN reserved_usd_micros BIGINT NOT NULL DEFAULT 0;

ALTER TABLE router.model_router_user_monthly_spend
    ADD COLUMN reserved_usd_micros BIGINT NOT NULL DEFAULT 0;

ALTER TABLE router.model_router_api_keys
    ADD COLUMN reserved_usd_micros BIGINT NOT NULL DEFAULT 0;

COMMIT;
