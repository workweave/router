BEGIN;

-- Router-owned source of truth for per-request human feedback collected via the
-- no-login feedback link (`/f/<token>`). One row per (installation, request):
-- the rating can be revised (thumbs up -> down or an edited comment), so writes
-- upsert on the natural key. The Weave backend keeps its own copy
-- (router_request_feedback) populated from the router.feedback OTLP span this
-- table's writes emit; this table is what the feedback page reads back so the
-- user sees their prior rating without a round-trip through Weave.
CREATE TABLE router.request_feedback (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    installation_id UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    -- Opaque Weave organization id copied from the routing request's external_id.
    -- Carried so the router.feedback span can attribute to the org without a
    -- join back to the installation at submit time.
    external_id     VARCHAR NOT NULL,
    request_id      VARCHAR NOT NULL,
    -- 'up' or 'down'. Stored as text (not an enum) to match the rest of the
    -- router schema's string-typed categoricals.
    rating          VARCHAR NOT NULL,
    -- Optional free-text comment; NULL when the user left it blank.
    comment         TEXT,
    -- How the feedback arrived: 'link' for the signed feedback page. Reserved
    -- for future in-product surfaces (e.g. CLI).
    source          VARCHAR NOT NULL DEFAULT 'link',
    -- Optional router user that submitted; NULL when the request was anonymous.
    router_user_id  UUID,
    UNIQUE (installation_id, request_id)
);

-- Feedback-page read path: latest rating for one request.
CREATE INDEX request_feedback_installation_id_request_id_idx
    ON router.request_feedback (installation_id, request_id);

COMMENT ON TABLE router.request_feedback IS 'Router-owned per-request human feedback captured via the no-login feedback link; mirrored into Weave via the router.feedback OTLP span';

COMMIT;
