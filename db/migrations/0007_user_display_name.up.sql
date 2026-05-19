BEGIN;

-- Adds a free-form display name to router users so the Weave dashboard can
-- render a human-readable label even when email is NULL (Claude CLI versions
-- that ship only account_uuid in metadata.user_id). Sourced from the
-- X-Weave-User-Name header that the installer plants from git user.name.
ALTER TABLE router.model_router_users
  ADD COLUMN display_name TEXT;

COMMENT ON COLUMN router.model_router_users.display_name IS
  'Free-form user display name (typically git user.name) carried on the X-Weave-User-Name request header. Nullable: requests without the header leave the column NULL; existing rows are not back-filled.';

COMMIT;
