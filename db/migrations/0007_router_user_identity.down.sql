BEGIN;

DROP INDEX router.model_router_api_keys_installation_active_unique;

-- The dedupe in the up migration is irreversible: we can't tell which keys
-- were "really" duplicates vs. legitimate per-user keys. Customers who relied
-- on the multi-key pattern must reissue keys after a rollback.

DROP TABLE router.model_router_users;

COMMIT;
