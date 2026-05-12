BEGIN;

-- Reverse the squashed initial schema. Drop order matters: tables with
-- FKs to model_router_installations are dropped first so the parent
-- can be dropped without ON DELETE bookkeeping.
--
-- We deliberately do NOT drop the `router` schema here. golang-migrate's
-- `schema_migrations` bookkeeping table lives in this schema (we pin it
-- via `?search_path=router` on the migrate URL), and migrate updates
-- that table *after* this down migration runs. Dropping the schema
-- would yank the table out from under migrate and abort the run. The
-- schema itself is created with `IF NOT EXISTS` in the up migration, so
-- leaving it as an empty namespace here is harmless and idempotent on
-- the next `up`.

DROP TABLE router.model_router_users;
DROP TABLE router.model_router_request_telemetry;
DROP TABLE router.session_pins;
DROP TABLE router.model_router_external_api_keys;
DROP TABLE router.model_router_api_keys;
DROP TABLE router.model_router_installations;

COMMIT;
