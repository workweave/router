-- Ensures the current UTC-month org spend row exists so a reserve UPDATE can
-- target it. No-op when the row is already present.
-- name: EnsureOrgMonthlySpendRow :exec
INSERT INTO router.organization_monthly_spend (organization_id, month, spent_usd_micros, reserved_usd_micros)
VALUES (
    @organization_id::varchar,
    DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date,
    0,
    0
)
ON CONFLICT (organization_id, month) DO NOTHING;

-- Atomically bumps org-month reserved when spent+reserved+amount still fits
-- under the configured org monthly limit. Returns the organization_id on
-- success; zero rows means limit reached (or no limit configured — caller
-- must skip reserve when limit is NULL before calling this).
-- name: TryBumpOrgMonthReserved :one
UPDATE router.organization_monthly_spend sp
SET reserved_usd_micros = sp.reserved_usd_micros + @amount_usd_micros::bigint,
    updated_at = NOW()
FROM router.organization_spend_limits lim
WHERE sp.organization_id = @organization_id::varchar
  AND sp.month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date
  AND lim.organization_id = sp.organization_id
  AND lim.org_monthly_limit_usd_micros IS NOT NULL
  AND sp.spent_usd_micros + sp.reserved_usd_micros + @amount_usd_micros::bigint
      <= lim.org_monthly_limit_usd_micros
RETURNING sp.organization_id;

-- name: EnsureUserMonthlySpendRow :exec
INSERT INTO router.model_router_user_monthly_spend (router_user_id, month, spent_usd_micros, reserved_usd_micros)
VALUES (
    @router_user_id::uuid,
    DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date,
    0,
    0
)
ON CONFLICT (router_user_id, month) DO NOTHING;

-- Bumps user-month reserved under the effective limit (per-user override when
-- present, else org default). Caller supplies the already-resolved effective
-- limit; NULL limit means the caller should skip this scope.
-- name: TryBumpUserMonthReserved :one
UPDATE router.model_router_user_monthly_spend sp
SET reserved_usd_micros = sp.reserved_usd_micros + @amount_usd_micros::bigint,
    updated_at = NOW()
WHERE sp.router_user_id = @router_user_id::uuid
  AND sp.month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date
  AND sp.spent_usd_micros + sp.reserved_usd_micros + @amount_usd_micros::bigint
      <= @limit_usd_micros::bigint
RETURNING sp.router_user_id;

-- Bumps api-key lifetime reserved under the key's spend_cap. Zero rows when
-- the key is missing, uncapped, or the bump would exceed the cap.
-- name: TryBumpAPIKeyReserved :one
UPDATE router.model_router_api_keys k
SET reserved_usd_micros = k.reserved_usd_micros + @amount_usd_micros::bigint
WHERE k.id = @api_key_id::uuid
  AND k.deleted_at IS NULL
  AND k.spend_cap_usd_micros IS NOT NULL
  AND k.spent_usd_micros + k.reserved_usd_micros + @amount_usd_micros::bigint
      <= k.spend_cap_usd_micros
RETURNING k.id;

-- name: InsertSpendReservation :one
INSERT INTO router.spend_reservations (
    scope_kind,
    scope_id,
    month,
    amount_usd_micros,
    expires_at,
    router_request_id
) VALUES (
    @scope_kind::varchar,
    @scope_id::varchar,
    sqlc.narg('month')::date,
    @amount_usd_micros::bigint,
    @expires_at::timestamptz,
    sqlc.narg('router_request_id')::varchar
)
RETURNING id, scope_kind, scope_id, month, amount_usd_micros, expires_at;

-- Atomic consume: DELETE … RETURNING is the sole settle/release/sweep
-- primitive. Zero rows = already released/settled/swept (idempotent no-op).
-- name: DeleteSpendReservation :one
DELETE FROM router.spend_reservations
WHERE id = @id::uuid
RETURNING id, scope_kind, scope_id, month, amount_usd_micros;

-- name: DecrementOrgMonthReserved :exec
UPDATE router.organization_monthly_spend
SET reserved_usd_micros = GREATEST(0, reserved_usd_micros - @amount_usd_micros::bigint),
    updated_at = NOW()
WHERE organization_id = @organization_id::varchar
  AND month = @month::date;

-- name: DecrementUserMonthReserved :exec
UPDATE router.model_router_user_monthly_spend
SET reserved_usd_micros = GREATEST(0, reserved_usd_micros - @amount_usd_micros::bigint),
    updated_at = NOW()
WHERE router_user_id = @router_user_id::uuid
  AND month = @month::date;

-- name: DecrementAPIKeyReserved :exec
UPDATE router.model_router_api_keys
SET reserved_usd_micros = GREATEST(0, reserved_usd_micros - @amount_usd_micros::bigint)
WHERE id = @api_key_id::uuid;

-- Deletes every expired reservation and returns the doomed rows so the
-- adapter can decrement denormalized reserved counters. Prefer calling
-- DeleteSpendReservation per id from Go when settling a known hold; this
-- batch path is for the TTL sweeper only.
-- name: DeleteExpiredSpendReservations :many
DELETE FROM router.spend_reservations
WHERE expires_at < @now::timestamptz
RETURNING id, scope_kind, scope_id, month, amount_usd_micros;
