-- Returns the org's current prepaid balance in USD micros. NULL is mapped to
-- a no-row error so callers can detect missing balance rows and decide
-- explicitly whether 0 or "no row" is the right answer (the middleware
-- treats both as a 402 candidate).
-- name: GetOrgCreditBalance :one
SELECT balance_usd_micros
FROM router.organization_credit_balance
WHERE organization_id = @organization_id::varchar;

-- Reports whether the org currently has an active billing override
-- (free credits, internal account, enterprise trial). Returns the matching
-- row id when present so callers can distinguish "no override" from a real
-- failure path. Uses a one-shot existence query rather than fetching the
-- override body — middleware only needs the boolean.
-- name: GetActiveBillingOverride :one
SELECT EXISTS (
    SELECT 1
    FROM router.organization_billing_overrides
    WHERE organization_id = @organization_id::varchar
      AND (expires_at IS NULL OR expires_at > NOW())
)::boolean AS has_override;

-- Atomic debit: decrement the balance and append a matching ledger row in a
-- single statement. delta_usd_micros is the signed change (negative for an
-- inference debit, zero for an override pass-through). notional_cost_micros
-- is always the would-be charge, populated for both override and real
-- debits so we keep a shadow billing trail.
--
-- No `balance >= amount` guard: concurrent requests can both pass the
-- preflight balance check and both debit; both debits must be recorded
-- even if the resulting balance is briefly negative. The min-balance
-- threshold on the middleware bounds the typical dip.
--
-- Returns the post-debit balance so middleware/log lines can report the
-- new value without a follow-up read.
--
-- When api_key_id is supplied, the same statement also bumps that key's
-- lifetime spent_usd_micros by the debit magnitude (-delta: the real cost on a
-- debit, zero on an override/subscription pass-through where delta is 0), so
-- per-key cap enforcement reads a single up-to-date row. The key_spend CTE is
-- data-modifying, so Postgres runs it to completion even though the final
-- SELECT does not reference it; it no-ops when api_key_id is NULL.
-- name: DebitOrgCredits :one
WITH updated AS (
    UPDATE router.organization_credit_balance
    SET balance_usd_micros = balance_usd_micros + @delta_usd_micros::bigint,
        updated_at = NOW()
    WHERE organization_id = @organization_id::varchar
    RETURNING balance_usd_micros
),
ledger AS (
    INSERT INTO router.organization_credit_ledger (
        organization_id,
        delta_usd_micros,
        notional_cost_micros,
        balance_after_micros,
        entry_type,
        router_request_id,
        router_model
    )
    SELECT
        @organization_id::varchar,
        @delta_usd_micros::bigint,
        @notional_cost_micros::bigint,
        updated.balance_usd_micros,
        @entry_type::varchar,
        sqlc.narg('router_request_id')::varchar,
        sqlc.narg('router_model')::varchar
    FROM updated
    RETURNING balance_after_micros
),
key_spend AS (
    -- delta is negative on a real debit, so subtracting it adds the spend
    -- magnitude; zero on override/subscription pass-throughs leaves it flat.
    -- Gated on `updated` producing a row: if the org balance row was missing
    -- (the debit no-ops and the app sees ErrBalanceRowMissing) we must NOT bump
    -- the key's lifetime spend, or a capped key could trip its cap with no
    -- matching ledger debit.
    UPDATE router.model_router_api_keys
    SET spent_usd_micros = spent_usd_micros - @delta_usd_micros::bigint
    WHERE id = sqlc.narg('api_key_id')::uuid
      AND EXISTS (SELECT 1 FROM updated)
)
SELECT balance_after_micros FROM ledger;

-- Paginated read for the dashboard ledger panel. Sorted newest-first so the
-- UI can render without an extra ORDER BY in Go.
-- name: ListOrgLedger :many
SELECT
    id,
    organization_id,
    delta_usd_micros,
    notional_cost_micros,
    balance_after_micros,
    entry_type,
    stripe_payment_intent_id,
    router_request_id,
    router_model,
    created_at
FROM router.organization_credit_ledger
WHERE organization_id = @organization_id::varchar
ORDER BY created_at DESC
LIMIT @row_limit::int;

-- Returns true if the three billing tables exist in the router schema. Used
-- by the router boot-time health check so a missing-migration state
-- disables billing rather than 500ing on every request.
-- name: CheckBillingTablesExist :one
SELECT (
    SELECT COUNT(*) FROM information_schema.tables
    WHERE table_schema = 'router'
      AND table_name IN (
        'organization_credit_balance',
        'organization_credit_ledger',
        'organization_billing_overrides'
      )
) = 3 AS billing_tables_exist;
