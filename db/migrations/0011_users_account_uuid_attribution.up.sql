BEGIN;

-- Allow attribution by Claude Code account_uuid when the inbound request
-- carries no email. Claude CLI v2.1.x packs only {device_id, account_uuid,
-- session_id} into metadata.user_id — there is no email field — so the
-- previous email-required design silently dropped attribution for every
-- request from those clients. account_uuid is stable per Claude account and
-- is enough to identify a seat for dashboard / cost-attribution purposes.

ALTER TABLE router.model_router_users
  ALTER COLUMN email DROP NOT NULL;

-- Backstop: a row with no identity signal is useless. The application
-- guarantees this (ResolveAndStashUser early-returns on both-empty input)
-- but the CHECK constraint keeps the invariant honest against any future
-- writer.
ALTER TABLE router.model_router_users
  ADD CONSTRAINT model_router_users_identity_present
  CHECK (email IS NOT NULL OR claude_account_uuid IS NOT NULL);

-- The pre-existing partial unique on (installation_id, email)
-- WHERE deleted_at IS NULL still applies. Postgres treats NULL as distinct
-- in unique indexes, so multiple email-NULL rows can coexist; the new
-- partial unique below scopes account-only rows so we don't accumulate
-- duplicates for the same Claude account on the same installation.
CREATE UNIQUE INDEX model_router_users_installation_account_unique
  ON router.model_router_users(installation_id, claude_account_uuid)
  WHERE email IS NULL
    AND claude_account_uuid IS NOT NULL
    AND deleted_at IS NULL;

COMMENT ON COLUMN router.model_router_users.email IS
  'Lowercased, trimmed user email (typically git user.email). Nullable: '
  'Claude CLI versions that send only account_uuid in metadata.user_id '
  'produce email-NULL rows keyed on (installation_id, claude_account_uuid).';

COMMIT;
