-- Creates a new model router installation.
-- name: CreateModelRouterInstallation :one
INSERT INTO router.model_router_installations (
    external_id,
    name,
    created_by,
    is_eval_allowlisted
)
VALUES (
    @external_id::varchar,
    @name::varchar,
    @created_by,
    @is_eval_allowlisted::boolean
)
RETURNING *;

-- Toggles the per-installation eval-override allowlist flag. Used by
-- the eval harness seeding flow (seed-key with --eval-allowlist) to
-- promote an installation without a redeploy. Scoped by
-- external_id so a mismatched (external_id, installation) pair refuses to
-- update instead of silently flipping a row in another tenant.
-- name: SetModelRouterInstallationEvalAllowlisted :exec
UPDATE router.model_router_installations
SET is_eval_allowlisted = @is_eval_allowlisted::boolean,
    updated_at           = NOW()
WHERE id = @id::uuid
  AND external_id = @external_id::varchar
  AND deleted_at IS NULL;

-- Gets an installation by id, scoped to an external_id to prevent cross-tenant access.
-- name: GetModelRouterInstallation :one
SELECT *
FROM router.model_router_installations
WHERE id = @id::uuid
  AND external_id = @external_id::varchar
  AND deleted_at IS NULL;

-- Lists active installations for an external_id.
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
