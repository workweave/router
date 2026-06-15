-- Upsert one request's feedback. The feedback link lets a user revise their
-- rating (flip thumb or edit the comment), so a repeat submission for the same
-- (installation, request) overwrites the prior row. Empty comments collapse to
-- NULL so "" and "no comment" are indistinguishable downstream.
-- name: UpsertRequestFeedback :exec
INSERT INTO router.request_feedback (
    installation_id,
    external_id,
    request_id,
    rating,
    comment,
    source,
    router_user_id,
    updated_at
) VALUES (
    @installation_id::uuid,
    @external_id::varchar,
    @request_id::varchar,
    @rating::varchar,
    NULLIF(sqlc.narg('comment')::text, ''),
    @source::varchar,
    sqlc.narg('router_user_id')::uuid,
    NOW()
)
ON CONFLICT (installation_id, request_id) DO UPDATE SET
    rating = EXCLUDED.rating,
    comment = EXCLUDED.comment,
    source = EXCLUDED.source,
    router_user_id = EXCLUDED.router_user_id,
    updated_at = NOW();

-- Read back one request's existing feedback for the feedback page so the user
-- sees their prior rating. Returns sql.ErrNoRows when none exists yet.
-- name: GetRequestFeedback :one
SELECT
    rating,
    comment
FROM router.request_feedback
WHERE installation_id = @installation_id::uuid
  AND request_id = @request_id::varchar;
