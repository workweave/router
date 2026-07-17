BEGIN;

ALTER TABLE router.organization_autopay_config
    DROP COLUMN monthly_recharge_cap_usd_micros,
    DROP COLUMN recharged_month,
    DROP COLUMN recharged_month_usd_micros;

COMMIT;
