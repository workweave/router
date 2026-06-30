BEGIN;

ALTER TABLE router.model_router_api_keys
    DROP COLUMN spent_usd_micros,
    DROP COLUMN spend_cap_usd_micros;

COMMIT;
