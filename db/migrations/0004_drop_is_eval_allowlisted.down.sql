BEGIN;
ALTER TABLE router.model_router_installations ADD COLUMN is_eval_allowlisted BOOLEAN NOT NULL DEFAULT false;
COMMIT;
