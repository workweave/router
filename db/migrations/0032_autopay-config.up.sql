BEGIN;

-- Per-org auto-recharge ("autopay") configuration and state machine. Lives in
-- the router schema next to the credit balance/ledger it governs; the router
-- only reads (enabled, threshold_usd_micros) in the debit hook to detect a
-- balance crossing, while the Weave control plane owns every write via raw
-- pgx (config upserts, the recharge state machine, backoff).
--
-- A row exists only once an org configures autopay. threshold_usd_micros is
-- the balance below which a recharge is triggered; recharge_usd_micros is the
-- credit grant applied per recharge. has_payment_method mirrors whether the
-- org's Stripe customer has a saved default card (UI gating only -- the charge
-- reads the live default_payment_method from Stripe).
CREATE TABLE router.organization_autopay_config (
    organization_id       VARCHAR(36) PRIMARY KEY,
    enabled               BOOLEAN     NOT NULL DEFAULT false,
    threshold_usd_micros  BIGINT      NOT NULL,
    recharge_usd_micros   BIGINT      NOT NULL,
    has_payment_method    BOOLEAN     NOT NULL DEFAULT false,
    state                 VARCHAR(32) NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','recharging','failing','disabled')),
    consecutive_failures  INT         NOT NULL DEFAULT 0,
    last_attempt_id       UUID,
    last_attempt_at       TIMESTAMPTZ,
    last_success_at       TIMESTAMPTZ,
    cooldown_until        TIMESTAMPTZ,
    created_by            VARCHAR(36),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Reconciliation-backstop predicate: enabled configs whose backoff cooldown
-- has elapsed. The control-plane sweep scans this to re-drive failed or missed
-- recharges; the partial index keeps it to the (small) enabled set.
CREATE INDEX organization_autopay_config_enabled_cooldown_idx
    ON router.organization_autopay_config (cooldown_until)
    WHERE enabled;

-- Low-balance email dedupe. Set when a warning email is sent; reset to NULL on
-- any top-up (manual or autopay) so the next depletion re-arms the warning.
-- Lives on the balance row it describes.
ALTER TABLE router.organization_credit_balance
    ADD COLUMN low_balance_notified_at TIMESTAMPTZ;

COMMIT;
