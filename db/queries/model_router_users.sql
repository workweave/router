-- Upserts an end-user identity keyed on (installation_id, email), refreshing
-- last_seen_at on every hit. claude_account_uuid is overwritten only when
-- the new value is non-NULL so a request from a non-Claude-Code client
-- can't blank out the field. Returns the row so the caller can stash
-- user_id on the request context.
-- name: UpsertModelRouterUserByEmail :one
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

-- Upserts an end-user identity keyed on (installation_id, claude_account_uuid)
-- for inbound requests that carry no email. Claude CLI v2.1.x ships only
-- {device_id, account_uuid, session_id} in metadata.user_id, so the
-- email-keyed upsert above would silently drop attribution for every such
-- request. The CONFLICT target matches the partial unique index
-- model_router_users_installation_account_unique
-- (WHERE email IS NULL AND claude_account_uuid IS NOT NULL).
-- name: UpsertModelRouterUserByAccountUUID :one
INSERT INTO router.model_router_users (
    installation_id,
    email,
    claude_account_uuid
)
VALUES (
    @installation_id::uuid,
    NULL,
    @claude_account_uuid::uuid
)
ON CONFLICT (installation_id, claude_account_uuid)
  WHERE email IS NULL AND claude_account_uuid IS NOT NULL AND deleted_at IS NULL
DO UPDATE SET last_seen_at = CURRENT_TIMESTAMP
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
