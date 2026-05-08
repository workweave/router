BEGIN;

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

COMMIT;
