BEGIN;
ALTER TABLE router.model_router_installations DROP COLUMN is_eval_allowlisted;
COMMIT;
