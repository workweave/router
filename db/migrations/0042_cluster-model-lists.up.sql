BEGIN;

CREATE TABLE router.cluster_model_lists (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id VARCHAR(255) NOT NULL,
    api_key_id    UUID NOT NULL REFERENCES router.model_router_api_keys(id) ON DELETE CASCADE,
    cluster_label VARCHAR(128) NOT NULL,
    models        TEXT[] NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Each key may configure each cluster at most once.
    UNIQUE (api_key_id, cluster_label),
    -- A cluster list must have at least one model.
    CONSTRAINT cluster_model_lists_models_not_empty CHECK (cardinality(models) > 0)
);

COMMIT;
