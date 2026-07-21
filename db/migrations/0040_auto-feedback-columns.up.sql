BEGIN;

ALTER TABLE router.router_feedback
    ADD COLUMN rating VARCHAR,
    ADD COLUMN suggested_label VARCHAR,
    ADD COLUMN source VARCHAR NOT NULL DEFAULT 'user';

COMMENT ON COLUMN router.router_feedback.rating IS '"up", "down", or NULL. Null means abstain or note-only (no verdict).';
COMMENT ON COLUMN router.router_feedback.suggested_label IS '"fast", "explore", "balanced", "high", or "maximum" — the complexity label the submitter thinks the turn needed. Set when rating is "down", NULL otherwise.';
COMMENT ON COLUMN router.router_feedback.source IS 'How the feedback was submitted: "user" (explicit /rf command), "auto" (automated judge at session stop).';

COMMIT;
