BEGIN;

ALTER TABLE router.model_router_users
  DROP COLUMN display_name;

COMMIT;
