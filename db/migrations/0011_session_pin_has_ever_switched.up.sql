BEGIN;

-- Latches true once a session has ever served two different models, set only
-- by UpdateSessionPinUsage when the just-served model differs from the prior
-- last_served_model. Anthropic thinking-block signatures are valid only for
-- the model that produced them, but Claude Code re-sends its full transcript
-- every turn, so the stale-signed blocks from a cross-model excursion persist
-- in the client history indefinitely. ModelSwitched alone strips them on the
-- single transition turn; this latch lets the emit path keep stripping on
-- every subsequent same-model turn for the life of the session, which is the
-- only window in which those poisoned blocks would otherwise reach Anthropic
-- and 400 with `Invalid signature in thinking block`.
ALTER TABLE router.session_pins
  ADD COLUMN has_ever_switched BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
