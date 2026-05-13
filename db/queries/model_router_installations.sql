-- name: CreateModelRouterInstallation :one
INSERT INTO router.model_router_installations (
    external_id,
    name,
    created_by
)
VALUES (
    @external_id::varchar,
    @name::varchar,
    @created_by
)
RETURNING *;

-- Gets an installation by id, scoped to an external_id to prevent cross-tenant access.
-- name: GetModelRouterInstallation :one
SELECT *
FROM router.model_router_installations
WHERE id = @id::uuid
  AND external_id = @external_id::varchar
  AND deleted_at IS NULL;

-- name: ListModelRouterInstallationsForExternalID :many
SELECT *
FROM router.model_router_installations
WHERE external_id = @external_id::varchar
  AND deleted_at IS NULL
ORDER BY created_at DESC;

-- Soft-deletes an installation, scoped to an external_id to prevent cross-tenant deletes.
-- name: SoftDeleteModelRouterInstallation :exec
UPDATE router.model_router_installations
SET deleted_at = NOW()
WHERE id = @id::uuid
  AND external_id = @external_id::varchar
  AND deleted_at IS NULL;

-- Replaces the per-installation model exclusion list, scoped to an external_id
-- to prevent cross-tenant updates. Empty array means "no exclusion". Bumps
-- updated_at so dashboards see the change.
-- name: UpdateModelRouterInstallationExcludedModels :exec
UPDATE router.model_router_installations
SET excluded_models = @excluded_models::text[],
    updated_at = NOW()
WHERE id = @id::uuid
  AND external_id = @external_id::varchar
  AND deleted_at IS NULL;
