BEGIN;
ALTER TABLE router.organization_credit_ledger DROP COLUMN memo;
COMMIT;
