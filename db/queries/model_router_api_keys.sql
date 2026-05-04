-- Creates a new model router API key bound to an installation.
-- name: CreateModelRouterAPIKey :one
INSERT INTO router.model_router_api_keys (
    installation_id,
    external_id,
    name,
    key_prefix,
    key_hash,
    key_suffix,
    created_by
)
VALUES (
    @installation_id::uuid,
    @external_id::varchar,
    @name,
    @key_prefix::varchar,
    @key_hash::varchar,
    @key_suffix::varchar,
    @created_by
)
RETURNING *;

-- Hot-path auth lookup. Matches a token's SHA-256 hash to an active api key plus its
-- active installation in a single indexed JOIN. Returns sql.ErrNoRows when nothing
-- matches; the auth middleware maps that to a 401. Filters out soft-deleted rows on
-- both sides so a soft-deleted installation invalidates all its keys without per-key
-- updates.
-- name: GetActiveModelRouterAPIKeyWithInstallationByHash :one
SELECT sqlc.embed(k), sqlc.embed(i)
FROM router.model_router_api_keys k
INNER JOIN router.model_router_installations i ON i.id = k.installation_id
WHERE k.key_hash = @key_hash::varchar
  AND k.deleted_at IS NULL
  AND i.deleted_at IS NULL;

-- Lists active keys for an installation (dashboard / CRUD; not on the request path).
-- name: ListModelRouterAPIKeysForInstallation :many
SELECT *
FROM router.model_router_api_keys
WHERE installation_id = @installation_id::uuid
  AND deleted_at IS NULL
ORDER BY created_at DESC;

-- Records that a key was used. Called fire-and-forget by the Service after successful
-- auth. Idempotent on retry.
-- name: MarkModelRouterAPIKeyUsed :exec
UPDATE router.model_router_api_keys
SET last_used_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;

-- Soft-deletes a key.
-- name: SoftDeleteModelRouterAPIKey :exec
UPDATE router.model_router_api_keys
SET deleted_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;
