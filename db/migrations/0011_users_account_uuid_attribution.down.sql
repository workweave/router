BEGIN;

DROP INDEX IF EXISTS router.model_router_users_installation_account_unique;

ALTER TABLE router.model_router_users
  DROP CONSTRAINT IF EXISTS model_router_users_identity_present;

-- Email-NULL rows would violate NOT NULL on rollback; delete them. They were
-- only created by the new account_uuid-only path, so dropping them is safe —
-- the workflow that created them no longer exists post-rollback.
DELETE FROM router.model_router_users WHERE email IS NULL;

ALTER TABLE router.model_router_users
  ALTER COLUMN email SET NOT NULL;

COMMENT ON COLUMN router.model_router_users.email IS
  'Lowercased, trimmed user email (typically git user.email). Application-normalized; column is plain TEXT.';

COMMIT;
