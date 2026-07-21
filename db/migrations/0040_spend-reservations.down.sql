BEGIN;

ALTER TABLE router.model_router_api_keys
    DROP COLUMN reserved_usd_micros;

ALTER TABLE router.model_router_user_monthly_spend
    DROP COLUMN reserved_usd_micros;

ALTER TABLE router.organization_monthly_spend
    DROP COLUMN reserved_usd_micros;

DROP TABLE router.spend_reservations;

COMMIT;
