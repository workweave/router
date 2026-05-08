BEGIN;

-- Adds the router-user identity layer (router.model_router_users) and
-- collapses the API-key model to one active key per installation. The two
-- changes ship together because they're the same logical move: identity
-- moves out of the API key (per-user keys) and into a first-class user row,
-- while the key itself becomes a plain installation-scoped secret.

CREATE TABLE router.model_router_users (
  id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  installation_id       UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
  email                 TEXT NOT NULL,
  claude_account_uuid   UUID,
  first_seen_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_seen_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  deleted_at            TIMESTAMP
);

CREATE INDEX model_router_users_installation_id_idx
  ON router.model_router_users(installation_id);

CREATE UNIQUE INDEX model_router_users_installation_email_unique
  ON router.model_router_users(installation_id, email)
  WHERE deleted_at IS NULL;

COMMENT ON TABLE router.model_router_users IS 'End-user identities seen on inbound requests, scoped to an installation. Replaces the per-user API key pattern.';
COMMENT ON COLUMN router.model_router_users.email IS 'Lowercased, trimmed user email (typically git user.email). Application-normalized; column is plain TEXT.';
COMMENT ON COLUMN router.model_router_users.claude_account_uuid IS 'Optional Claude Code account_uuid carried in metadata.user_id; informational only.';

-- For any installation that holds more than one active key today, keep the
-- most-recently-used row (or the most recently created if no key has ever
-- been used) and soft-delete the rest. The newest wins on the assumption
-- that customers rotate forward when they want a new key.
WITH ranked AS (
  SELECT
    id,
    ROW_NUMBER() OVER (
      PARTITION BY installation_id
      ORDER BY COALESCE(last_used_at, created_at) DESC, created_at DESC, id DESC
    ) AS rn
  FROM router.model_router_api_keys
  WHERE deleted_at IS NULL
)
UPDATE router.model_router_api_keys
SET deleted_at = CURRENT_TIMESTAMP
WHERE id IN (SELECT id FROM ranked WHERE rn > 1);

CREATE UNIQUE INDEX model_router_api_keys_installation_active_unique
  ON router.model_router_api_keys(installation_id)
  WHERE deleted_at IS NULL;

COMMENT ON INDEX router.model_router_api_keys_installation_active_unique IS
  'One active key per installation. Identity is carried by router.model_router_users; a key is an installation-scoped secret, not a per-user credential.';

COMMIT;
