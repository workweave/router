BEGIN;

-- Closes the per-user-API-key pattern: keys are now an installation-scoped
-- secret; identity moves into router.model_router_users (migration 0006).
--
-- For any installation that holds more than one active key, keep the
-- most-recently-used row (or the most recently created if no key has
-- ever been used) and soft-delete the rest. The newest key wins on the
-- assumption that customers rotate forward when they want a new key.
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
