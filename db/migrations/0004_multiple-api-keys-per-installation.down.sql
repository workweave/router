BEGIN;

-- Collapse to one active key per installation before re-adding the partial
-- unique index. Without this step the CREATE UNIQUE INDEX below would fail
-- with a 23505 on any installation that issued a second key while the up
-- migration was in force. Newest-created wins; older active rows are
-- soft-deleted so the audit trail is preserved.
WITH ranked AS (
    SELECT id,
           row_number() OVER (PARTITION BY installation_id ORDER BY created_at DESC, id DESC) AS rn
    FROM router.model_router_api_keys
    WHERE deleted_at IS NULL
)
UPDATE router.model_router_api_keys
SET deleted_at = NOW()
FROM ranked
WHERE ranked.id = router.model_router_api_keys.id
  AND ranked.rn > 1;

CREATE UNIQUE INDEX model_router_api_keys_installation_active_unique
  ON router.model_router_api_keys(installation_id)
  WHERE deleted_at IS NULL;

COMMENT ON INDEX router.model_router_api_keys_installation_active_unique IS
  'One active key per installation. Identity is carried by router.model_router_users; a key is an installation-scoped secret, not a per-user credential.';

COMMENT ON TABLE router.model_router_api_keys IS 'Rotatable bearer keys (rk_ prefix) per installation';

COMMIT;
