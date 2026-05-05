-- Creates a new external API key for an installation. The unique index on
-- (installation_id, provider) WHERE deleted_at IS NULL enforces one key per
-- provider; callers must soft-delete the existing key before inserting.
-- name: CreateExternalAPIKey :one
INSERT INTO router.model_router_external_api_keys (
    installation_id,
    external_id,
    provider,
    key_ciphertext,
    key_prefix,
    key_suffix,
    key_fingerprint,
    name,
    created_by
)
VALUES (
    @installation_id::uuid,
    @external_id::varchar,
    @provider::varchar,
    @key_ciphertext::bytea,
    @key_prefix::varchar,
    @key_suffix::varchar,
    @key_fingerprint::varchar,
    @name,
    @created_by
)
RETURNING *;

-- Returns all active external API keys for an installation. Used by the auth
-- cache to populate the ExternalKeys map on cache miss.
-- name: GetActiveExternalAPIKeysForInstallation :many
SELECT *
FROM router.model_router_external_api_keys
WHERE installation_id = @installation_id::uuid
  AND deleted_at IS NULL
ORDER BY provider, created_at DESC;

-- Returns all active external API keys for listing in the UI.
-- name: ListActiveExternalAPIKeysForInstallation :many
SELECT *
FROM router.model_router_external_api_keys
WHERE installation_id = @installation_id::uuid
  AND deleted_at IS NULL
ORDER BY provider, created_at DESC;

-- Soft-deletes an external API key. Cross-tenant safe via installation_id predicate.
-- name: SoftDeleteExternalAPIKey :exec
UPDATE router.model_router_external_api_keys
SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE id = @id::uuid
  AND installation_id = @installation_id::uuid
  AND deleted_at IS NULL;

-- Soft-deletes the existing key for a provider before upsert.
-- name: SoftDeleteExternalAPIKeyByProvider :exec
UPDATE router.model_router_external_api_keys
SET deleted_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP
WHERE installation_id = @installation_id::uuid
  AND provider = @provider::varchar
  AND deleted_at IS NULL;

-- Fire-and-forget update after successful upstream call.
-- name: MarkExternalAPIKeyUsed :exec
UPDATE router.model_router_external_api_keys
SET last_used_at = NOW()
WHERE id = @id::uuid
  AND deleted_at IS NULL;
