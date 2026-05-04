BEGIN;
DROP TABLE router.model_router_api_keys;
DROP TABLE router.model_router_installations;
-- We deliberately do NOT drop the `router` schema here. golang-migrate's
-- `schema_migrations` bookkeeping table lives in this schema (we pin it via
-- `?search_path=router` on the migrate URL), and migrate updates that table
-- *after* this down migration runs. Dropping the schema would yank the table
-- out from under migrate and abort the run. The schema itself is created with
-- `IF NOT EXISTS` in the up migration, so leaving it as an empty namespace
-- here is harmless and idempotent on the next `up`.
COMMIT;
