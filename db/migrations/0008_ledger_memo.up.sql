BEGIN;
ALTER TABLE router.organization_credit_ledger ADD COLUMN memo TEXT;
COMMIT;
