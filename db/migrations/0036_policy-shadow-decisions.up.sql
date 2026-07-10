BEGIN;

CREATE TABLE router.policy_shadow_decisions (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installation_id                 UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    organization_id                 VARCHAR,
    rollout_id                      VARCHAR,
    client_app                      VARCHAR,
    training_allowed                BOOLEAN NOT NULL,
    serving_strategy                VARCHAR NOT NULL,
    serving_model                   VARCHAR NOT NULL,
    serving_provider                VARCHAR NOT NULL,
    serving_route_id                VARCHAR,
    serving_policy_route_key        VARCHAR,
    serving_policy_artifact_id      VARCHAR,
    serving_policy_artifact_sha256  VARCHAR,
    shadow_strategy                 VARCHAR NOT NULL,
    shadow_model                    VARCHAR,
    shadow_provider                 VARCHAR,
    shadow_route_id                 VARCHAR,
    shadow_policy_route_key         VARCHAR,
    shadow_policy_artifact_id       VARCHAR,
    shadow_policy_artifact_sha256   VARCHAR,
    shadow_latency_ms               BIGINT NOT NULL,
    shadow_error                    VARCHAR,
    models_agree                    BOOLEAN NOT NULL
);

CREATE INDEX policy_shadow_decisions_installation_created_at_idx
    ON router.policy_shadow_decisions (installation_id, created_at DESC);
CREATE INDEX policy_shadow_decisions_rollout_id_idx
    ON router.policy_shadow_decisions (rollout_id)
    WHERE rollout_id IS NOT NULL;

COMMENT ON TABLE router.policy_shadow_decisions IS
    'Content-free serving-vs-shadow policy comparisons; shadow decisions never dispatch or enter online learning';

COMMIT;
