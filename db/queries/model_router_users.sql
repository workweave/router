-- Upserts an end-user identity for an installation, refreshing last_seen_at on every
-- hit. claude_account_uuid is overwritten only when the new value is non-NULL so a
-- request from a non-Claude-Code client can't blank out the field. Returns the row
-- so the caller can stash user_id on the request context.
-- name: UpsertModelRouterUser :one
INSERT INTO router.model_router_users (
    installation_id,
    email,
    claude_account_uuid
)
VALUES (
    @installation_id::uuid,
    @email::text,
    sqlc.narg('claude_account_uuid')::uuid
)
ON CONFLICT (installation_id, email) WHERE deleted_at IS NULL DO UPDATE SET
    last_seen_at        = CURRENT_TIMESTAMP,
    claude_account_uuid = COALESCE(EXCLUDED.claude_account_uuid, router.model_router_users.claude_account_uuid)
RETURNING *;

-- Single-row read by id; returns sql.ErrNoRows when missing or soft-deleted.
-- name: GetModelRouterUser :one
SELECT *
FROM router.model_router_users
WHERE id = @id::uuid
  AND deleted_at IS NULL;

-- Lists active users for an installation (admin / dashboard; not on the request path).
-- name: ListModelRouterUsersForInstallation :many
SELECT *
FROM router.model_router_users
WHERE installation_id = @installation_id::uuid
  AND deleted_at IS NULL
ORDER BY last_seen_at DESC;
