BEGIN;

-- Records the other half of the band pair the cluster scorer picked on this
-- session's first turn: the runner-up (second-best) model and its provider.
-- Written only on the pin's first insert and preserved across every later
-- upsert (the ON CONFLICT set in UpsertSessionPin deliberately omits these two
-- columns, same as installation_id), so the pair stays frozen for the
-- conversation's life. Empty string for pins created outside the scorer path
-- (force-model, loop-break) or when only one model was eligible. A later
-- per-turn policy reads them to swap between the pinned model and its pair
-- without re-running the scorer.
ALTER TABLE router.session_pins
    ADD COLUMN paired_provider VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN paired_model    VARCHAR(128) NOT NULL DEFAULT '';

COMMIT;
