BEGIN;

ALTER TABLE router.organization_credit_balance
    DROP COLUMN low_balance_notified_at;

DROP TABLE router.organization_autopay_config;

COMMIT;
