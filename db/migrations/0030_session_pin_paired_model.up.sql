BEGIN;

-- Records the other half of the band pair the cluster scorer picks: the
-- runner-up (second-best) model and its provider. Refreshed whenever a genuine
-- scorer re-run supplies a fresh runner-up (first turn, switch, expired-pin
-- re-route) and preserved on sticky refreshes / reconstructed re-anchors (see
-- the ON CONFLICT logic in UpsertSessionPin), so the pair always matches the
-- live routing decision and never collapses onto the pinned model. Empty string
-- for pins created outside the scorer path (force-model, loop-break) or when
-- only one model was eligible. A later per-turn policy reads them to swap
-- between the pinned model and its pair without re-running the scorer.
ALTER TABLE router.session_pins
    ADD COLUMN paired_provider VARCHAR(32) NOT NULL DEFAULT '',
    ADD COLUMN paired_model    VARCHAR(128) NOT NULL DEFAULT '';

COMMIT;
