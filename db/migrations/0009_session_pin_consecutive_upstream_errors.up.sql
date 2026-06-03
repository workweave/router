BEGIN;

-- Counts consecutive turns ending in non-retryable upstream errors (4xx
-- other than 408/429) for a sticky-pinned session. The turn loop resets
-- this column to 0 on any successful turn and evicts the pin when the
-- count reaches the two-strike threshold, so a session can't wedge on a
-- bad sticky decision the way it did when we observed four consecutive
-- 400s on a gpt-5.5 pin with no auto-recovery.
ALTER TABLE router.session_pins
  ADD COLUMN consecutive_upstream_errors INTEGER NOT NULL DEFAULT 0;

COMMIT;
