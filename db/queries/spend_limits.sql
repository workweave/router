-- Reads the org's spend-limit configuration. Both limits are nullable:
-- NULL means that limit kind is not configured. No row means neither is.
-- name: GetOrgSpendLimits :one
SELECT user_monthly_limit_usd_micros, org_monthly_limit_usd_micros
FROM router.organization_spend_limits
WHERE organization_id = @organization_id::varchar;

-- Resolves a user's effective monthly limit and current-month spend in one
-- round trip for the request-path gate. effective_limit_usd_micros is the
-- per-user override when an override row exists (its NULL means "explicitly
-- uncapped"), otherwise the org-wide default; has_override distinguishes the
-- two NULL meanings. spent is 0 when the user has no spend row this month.
-- Spend/override subqueries require the user to belong to an installation
-- whose external_id matches @organization_id (#796); a mismatched pair is a
-- silent miss (spent 0, no override). Org default still resolves for the org.
-- name: GetUserMonthlySpendAndLimit :one
SELECT
    (EXISTS (
        SELECT 1 FROM router.model_router_user_spend_limits ovr
        JOIN router.model_router_users u ON u.id = ovr.router_user_id
        JOIN router.model_router_installations i ON i.id = u.installation_id
        WHERE ovr.router_user_id = @router_user_id::uuid
          AND i.external_id = @organization_id::varchar
    ))::boolean AS has_override,
    (SELECT ovr.monthly_limit_usd_micros
     FROM router.model_router_user_spend_limits ovr
     JOIN router.model_router_users u ON u.id = ovr.router_user_id
     JOIN router.model_router_installations i ON i.id = u.installation_id
     WHERE ovr.router_user_id = @router_user_id::uuid
       AND i.external_id = @organization_id::varchar) AS override_limit_usd_micros,
    (SELECT lim.user_monthly_limit_usd_micros
     FROM router.organization_spend_limits lim
     WHERE lim.organization_id = @organization_id::varchar) AS org_default_limit_usd_micros,
    COALESCE((
        SELECT sp.spent_usd_micros
        FROM router.model_router_user_monthly_spend sp
        JOIN router.model_router_users u ON u.id = sp.router_user_id
        JOIN router.model_router_installations i ON i.id = u.installation_id
        WHERE sp.router_user_id = @router_user_id::uuid
          AND i.external_id = @organization_id::varchar
          AND sp.month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date
    ), 0)::bigint AS spent_usd_micros;

-- Reads the org's month-to-date spend alongside its monthly cap for the
-- org-wide gate. Zero-spend months have no row; COALESCE keeps the read
-- total.
-- name: GetOrgMonthlySpendAndLimit :one
SELECT
    (SELECT lim.org_monthly_limit_usd_micros
     FROM router.organization_spend_limits lim
     WHERE lim.organization_id = @organization_id::varchar) AS org_limit_usd_micros,
    COALESCE((
        SELECT sp.spent_usd_micros
        FROM router.organization_monthly_spend sp
        WHERE sp.organization_id = @organization_id::varchar
          AND sp.month = DATE_TRUNC('month', NOW() AT TIME ZONE 'utc')::date
    ), 0)::bigint AS spent_usd_micros;
