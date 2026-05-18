-- Upserts an end-user identity keyed on (installation_id, email), refreshing
-- last_seen_at on every hit. claude_account_uuid and display_name are
-- overwritten only when the new value is non-NULL so a request from a
-- non-Claude-Code client (or one that omits the X-Weave-User-Name header)
-- can't blank out fields populated by earlier requests. Returns the row so
-- the caller can stash user_id on the request context.
--
-- Merge path: when claude_account_uuid is non-NULL and a UUID-only orphan
-- already exists for this account (created by an earlier email-less request
-- via UpsertModelRouterUserByAccountUUID), we update that row's email +
-- display_name in place instead of inserting a new email-keyed row. Without
-- this, every installer upgrade that introduces X-Weave-User-Email creates
-- duplicate rows for the same human and the dashboard picker shows both.
-- The fallback INSERT keeps the original ON CONFLICT path so concurrent
-- email-bearing requests still collapse onto a single row.
-- name: UpsertModelRouterUserByEmail :one
WITH merged AS (
    UPDATE router.model_router_users
    SET email        = @email::text,
        display_name = COALESCE(sqlc.narg('display_name')::text, display_name),
        last_seen_at = CURRENT_TIMESTAMP
    WHERE installation_id     = @installation_id::uuid
      AND claude_account_uuid = sqlc.narg('claude_account_uuid')::uuid
      AND email IS NULL
      AND deleted_at IS NULL
    RETURNING *
),
inserted AS (
    INSERT INTO router.model_router_users (
        installation_id,
        email,
        claude_account_uuid,
        display_name
    )
    SELECT
        @installation_id::uuid,
        @email::text,
        sqlc.narg('claude_account_uuid')::uuid,
        sqlc.narg('display_name')::text
    WHERE NOT EXISTS (SELECT 1 FROM merged)
    ON CONFLICT (installation_id, email) WHERE deleted_at IS NULL DO UPDATE SET
        last_seen_at        = CURRENT_TIMESTAMP,
        claude_account_uuid = COALESCE(EXCLUDED.claude_account_uuid, router.model_router_users.claude_account_uuid),
        display_name        = COALESCE(EXCLUDED.display_name, router.model_router_users.display_name)
    RETURNING *
)
SELECT * FROM merged
UNION ALL
SELECT * FROM inserted;

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
    claude_account_uuid,
    display_name
)
VALUES (
    @installation_id::uuid,
    NULL,
    @claude_account_uuid::uuid,
    sqlc.narg('display_name')::text
)
ON CONFLICT (installation_id, claude_account_uuid)
  WHERE email IS NULL AND claude_account_uuid IS NOT NULL AND deleted_at IS NULL
DO UPDATE SET
    last_seen_at = CURRENT_TIMESTAMP,
    display_name = COALESCE(EXCLUDED.display_name, router.model_router_users.display_name)
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
