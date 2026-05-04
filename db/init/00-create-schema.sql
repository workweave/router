-- Postgres init script (mounted into /docker-entrypoint-initdb.d). Runs once on
-- first database initialization, before golang-migrate connects. Pre-creating
-- the schema is necessary because the migrate sidecar runs with
-- `search_path=router`, which would otherwise have no schema to land
-- `schema_migrations` (the migrate bookkeeping table) into.
CREATE SCHEMA IF NOT EXISTS router;
