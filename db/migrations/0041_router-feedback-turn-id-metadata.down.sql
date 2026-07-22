BEGIN;
ALTER TABLE router.router_feedback DROP COLUMN request_id, DROP COLUMN route_id;
COMMIT;
