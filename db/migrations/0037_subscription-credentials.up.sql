BEGIN;

CREATE TABLE router.subscription_credentials (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id          UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    external_id              VARCHAR(64) NOT NULL,
    user_email               VARCHAR(320) NOT NULL,
    provider                 VARCHAR(32) NOT NULL CHECK (provider IN ('anthropic', 'openai')),
    account_label            VARCHAR(255),
    account_fingerprint      VARCHAR(64) NOT NULL,
    chatgpt_account_id       VARCHAR(128),
    access_token_ciphertext  BYTEA NOT NULL,
    refresh_token_ciphertext BYTEA NOT NULL,
    access_token_expires_at  TIMESTAMPTZ,
    last_refreshed_at        TIMESTAMPTZ,
    last_used_at             TIMESTAMPTZ,
    refresh_failed_at        TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at               TIMESTAMPTZ,
    created_by               VARCHAR(64)
);

CREATE INDEX subscription_credentials_pool_idx
    ON router.subscription_credentials (installation_id, user_email, provider, created_at)
    WHERE deleted_at IS NULL;

CREATE UNIQUE INDEX subscription_credentials_account_unique
    ON router.subscription_credentials (installation_id, user_email, provider, account_fingerprint)
    WHERE deleted_at IS NULL;

COMMENT ON TABLE router.subscription_credentials IS
    'Per-user pool of enrolled Claude/ChatGPT subscription OAuth credentials; tokens encrypted (Tink AEAD), rotated through as each account exhausts its plan window';

COMMIT;
