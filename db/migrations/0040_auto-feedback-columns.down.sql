BEGIN;

ALTER TABLE router.router_feedback
    DROP COLUMN source,
    DROP COLUMN suggested_label,
    DROP COLUMN rating;

COMMIT;
