BEGIN;

ALTER TABLE router.model_router_installations
  RENAME COLUMN external_id TO organization_id;

ALTER TABLE router.model_router_api_keys
  RENAME COLUMN external_id TO organization_id;

ALTER INDEX model_router_installations_external_id_idx
  RENAME TO model_router_installations_organization_id_idx;

ALTER INDEX model_router_api_keys_external_id_idx
  RENAME TO model_router_api_keys_organization_id_idx;

ALTER INDEX model_router_installations_name_external_id_unique
  RENAME TO model_router_installations_name_org_unique;

COMMIT;
