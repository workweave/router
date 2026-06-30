BEGIN;

-- Per-key lifetime spend cap for budgeted virtual keys. A key with a
-- non-NULL cap is rejected once its cumulative spend reaches the cap; NULL
-- means uncapped (the default for every existing key). spent_usd_micros is a
-- running lifetime total bumped in the same transaction as the org debit, so
-- enforcement reads a single indexed row instead of summing telemetry.
ALTER TABLE router.model_router_api_keys
    ADD COLUMN spend_cap_usd_micros BIGINT,
    ADD COLUMN spent_usd_micros BIGINT NOT NULL DEFAULT 0;

COMMIT;
