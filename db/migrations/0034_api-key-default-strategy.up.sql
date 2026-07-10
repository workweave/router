BEGIN;

-- Per-key routing strategy default. Lets a client that cannot send the
-- x-weave-router-strategy header (e.g. Cursor's Override Base URL, which has
-- no custom-header field) opt into a non-cluster strategy by using a
-- dedicated key instead. NULL means "no key default" -- the deployment
-- default (cluster) applies. The x-weave-router-strategy header, when
-- present and recognized, always takes precedence over this column; see
-- WithRouterStrategyOverride. Allowed values (enforced in the app layer, not
-- a CHECK constraint, to match the header's own validation path):
-- cluster | rl | hmm | bandit.
ALTER TABLE router.model_router_api_keys
    ADD COLUMN default_strategy TEXT;

COMMIT;
