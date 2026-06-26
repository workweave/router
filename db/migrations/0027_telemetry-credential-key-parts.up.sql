BEGIN;

-- Cross-user subscription-usage attribution. A turn served on a caller's own
-- Claude/Codex subscription forwards that subscription token upstream, but the
-- row is attributed (router_user_id) by the caller's git identity header --
-- the two are unrelated, and the token itself was never recorded. So we cannot
-- today tell whose subscription actually paid for a turn, only whose git
-- identity issued it: a token shared across developers (one Claude account, many
-- seats) is invisible.
--
-- credential_key_prefix + credential_key_suffix store the same safe display
-- form already used for API-key observability (first 8 chars + last 4 chars,
-- never the full token). With these columns, "one credential, many
-- router_user_ids" becomes a direct GROUP BY rather than a metadata inference.
-- credential_source records which precedence branch the credential came from
-- (subscription / codex_subscription / byok / client). All three are NULL when
-- no per-request credential was resolved (deployment-key turns).
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN credential_key_prefix VARCHAR,
    ADD COLUMN credential_key_suffix VARCHAR,
    ADD COLUMN credential_source     VARCHAR;

COMMENT ON COLUMN router.model_router_request_telemetry.credential_key_prefix IS
    'Safe display prefix (first 8 characters) of the upstream credential that served the turn. NULL on deployment-key turns.';
COMMENT ON COLUMN router.model_router_request_telemetry.credential_key_suffix IS
    'Safe display suffix (last 4 characters) of the upstream credential that served the turn. NULL on deployment-key turns or very short credentials.';
COMMENT ON COLUMN router.model_router_request_telemetry.credential_source IS
    'Which credential precedence branch served the turn: subscription, codex_subscription, byok, or client. NULL on deployment-key turns.';

-- Recreate the production-traffic view so the new columns surface through it
-- (CREATE VIEW ... SELECT * freezes its column list at creation). Body
-- unchanged from migration 0023.
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
