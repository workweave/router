-- Creates a new subscription credential in a user's pool. The unique index on
-- (installation_id, user_email, provider, account_fingerprint) WHERE deleted_at
-- IS NULL enforces one row per enrolled account; callers soft-delete by
-- fingerprint before inserting to replace tokens on re-enrollment.
-- name: CreateSubscriptionCredential :one
INSERT INTO router.subscription_credentials (
    installation_id,
    external_id,
    user_email,
    provider,
    account_label,
    account_fingerprint,
    chatgpt_account_id,
    access_token_ciphertext,
    refresh_token_ciphertext,
    access_token_expires_at,
    created_by
)
VALUES (
    @installation_id::uuid,
    @external_id::varchar,
    @user_email::varchar,
    @provider::varchar,
    @account_label,
    @account_fingerprint::varchar,
    @chatgpt_account_id,
    @access_token_ciphertext::bytea,
    @refresh_token_ciphertext::bytea,
    @access_token_expires_at,
    @created_by
)
RETURNING *;

-- Returns a user's usable pool for rotation, oldest-enrolled first (stable
-- rotation order). Excludes credentials whose refresh terminally failed —
-- those need re-enrollment before they can serve again.
-- name: GetActiveSubscriptionCredentialsForUser :many
SELECT *
FROM router.subscription_credentials
WHERE installation_id = @installation_id::uuid
  AND user_email = @user_email::varchar
  AND deleted_at IS NULL
  AND refresh_failed_at IS NULL
ORDER BY provider, created_at;

-- Returns every non-deleted credential for a user, including refresh-failed
-- ones, for the enrollment listing endpoint.
-- name: ListSubscriptionCredentialsForUser :many
SELECT *
FROM router.subscription_credentials
WHERE installation_id = @installation_id::uuid
  AND user_email = @user_email::varchar
  AND deleted_at IS NULL
ORDER BY provider, created_at;

-- Persists rotated tokens after a successful refresh and clears any earlier
-- terminal-failure mark.
-- name: UpdateSubscriptionCredentialTokens :exec
UPDATE router.subscription_credentials
SET access_token_ciphertext = @access_token_ciphertext::bytea,
    refresh_token_ciphertext = @refresh_token_ciphertext::bytea,
    access_token_expires_at = @access_token_expires_at,
    last_refreshed_at = NOW(),
    refresh_failed_at = NULL,
    updated_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;

-- Marks a credential's refresh as terminally failed (4xx from the token
-- endpoint); the pool skips it until the user re-enrolls.
-- name: MarkSubscriptionCredentialRefreshFailed :exec
UPDATE router.subscription_credentials
SET refresh_failed_at = NOW(), updated_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;

-- Fire-and-forget update after a pooled credential serves a turn.
-- name: MarkSubscriptionCredentialUsed :exec
UPDATE router.subscription_credentials
SET last_used_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;

-- Soft-deletes one credential. Cross-tenant safe via installation_id and
-- user_email predicates; a foreign id matches zero rows.
-- name: SoftDeleteSubscriptionCredential :execrows
UPDATE router.subscription_credentials
SET deleted_at = NOW(), updated_at = NOW()
WHERE id = @id::uuid
  AND installation_id = @installation_id::uuid
  AND user_email = @user_email::varchar
  AND deleted_at IS NULL;

-- Soft-deletes the existing row for an enrolled account before re-inserting
-- fresh tokens (replace-on-re-enrollment).
-- name: SoftDeleteSubscriptionCredentialByFingerprint :exec
UPDATE router.subscription_credentials
SET deleted_at = NOW(), updated_at = NOW()
WHERE installation_id = @installation_id::uuid
  AND user_email = @user_email::varchar
  AND provider = @provider::varchar
  AND account_fingerprint = @account_fingerprint::varchar
  AND deleted_at IS NULL;
