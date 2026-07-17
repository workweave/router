BEGIN;

-- Monthly cap on autopay recharges, written by the Weave control plane like
-- the rest of this table. NULL cap means uncapped. recharged_month /
-- recharged_month_usd_micros track successful autopay grants in the current
-- UTC calendar month (recharged_month is the month's first day); the claim
-- flow refuses a recharge that would push the month's total past the cap,
-- pausing autopay until the month rolls over or an admin raises the cap.
ALTER TABLE router.organization_autopay_config
    ADD COLUMN monthly_recharge_cap_usd_micros BIGINT,
    ADD COLUMN recharged_month                 DATE,
    ADD COLUMN recharged_month_usd_micros      BIGINT NOT NULL DEFAULT 0;

COMMIT;
