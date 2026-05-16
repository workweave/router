BEGIN;

-- Prepaid credit balance, one row per Weave organization. organization_id is
-- the opaque external identifier set on auth.installation.external_id; the
-- router schema deliberately has no FK to public.organizations.
CREATE TABLE router.organization_credit_balance (
    organization_id    VARCHAR(36) PRIMARY KEY,
    balance_usd_micros BIGINT      NOT NULL DEFAULT 0,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Append-only history of grants, debits, and adjustments. delta_usd_micros is
-- positive for grants and zero for override (free) inferences; debits store
-- the negative charge. notional_cost_micros is the would-be charge regardless
-- of override status, so we can answer "what would we have billed if the
-- override were off."
CREATE TABLE router.organization_credit_ledger (
    id                       UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id          VARCHAR(36) NOT NULL,
    delta_usd_micros         BIGINT      NOT NULL,
    notional_cost_micros     BIGINT      NOT NULL DEFAULT 0,
    balance_after_micros     BIGINT      NOT NULL,
    entry_type               VARCHAR(32) NOT NULL CHECK (entry_type IN ('topup','inference','refund','adjustment')),
    stripe_payment_intent_id VARCHAR(255),
    router_request_id        VARCHAR(64),
    router_model             VARCHAR(128),
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX organization_credit_ledger_org_created_at_idx
    ON router.organization_credit_ledger (organization_id, created_at DESC);

-- Stripe webhook retries fire the same checkout.session.completed event
-- multiple times; the partial unique index gives us idempotency without
-- blocking inference rows that have no payment_intent.
CREATE UNIQUE INDEX organization_credit_ledger_stripe_pi_idx
    ON router.organization_credit_ledger (stripe_payment_intent_id)
    WHERE stripe_payment_intent_id IS NOT NULL;

-- Orgs that bypass billing entirely (Weave internal, enterprise trials,
-- comped accounts). The middleware passes them through; the debit hook still
-- writes a ledger row with delta=0 and notional_cost_micros>0 so we keep a
-- shadow billing trail for capacity planning.
CREATE TABLE router.organization_billing_overrides (
    organization_id VARCHAR(36) PRIMARY KEY,
    reason          TEXT        NOT NULL,
    expires_at      TIMESTAMPTZ,
    created_by      VARCHAR(36),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMIT;
