BEGIN;

-- Records the model that actually served the previous upstream turn for this
-- session, written only by UpdateSessionPinUsage (off the request path, after
-- the turn completes) — never by UpsertSessionPin. Keeping it out of the upsert
-- means a /force-model write does not clobber the genuinely-last-served model,
-- so the next turn can still detect that the model changed and strip stale
-- Anthropic thinking-block signatures that the new model would reject with 400.
ALTER TABLE router.session_pins
    ADD COLUMN last_served_model VARCHAR NOT NULL DEFAULT '';

COMMIT;
