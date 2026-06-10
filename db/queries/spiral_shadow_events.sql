-- Records one shadow-mode spiral detection. Written at detection time with
-- context.Background() (the request ctx may already be canceled); fire rate,
-- precision, and lead time are computed offline by joining session_key
-- against model_router_request_telemetry / session outcomes.
-- name: InsertSpiralShadowEvent :exec
INSERT INTO router.spiral_shadow_events (
    installation_id,
    session_key,
    role,
    routed_model,
    turn_type,
    reason,
    err_streak,
    errored_results,
    tool_results,
    max_same_file_edits,
    same_file_path_hash,
    repeat_frac,
    monologue_len,
    tool_call_count,
    message_count
) VALUES (
    @installation_id::uuid,
    @session_key::bytea,
    @role::varchar,
    @routed_model::varchar,
    @turn_type::varchar,
    @reason::varchar,
    @err_streak::int,
    @errored_results::int,
    @tool_results::int,
    @max_same_file_edits::int,
    @same_file_path_hash::varchar,
    @repeat_frac::double precision,
    @monologue_len::int,
    @tool_call_count::int,
    @message_count::int
);

-- Once-per-(session, role, reason) budget check: any prior event means this
-- signal class already fired for this session and must not produce another
-- row, even across replicas and in-proc cache expiry.
-- name: CountSpiralShadowEvents :one
SELECT COUNT(*)
FROM router.spiral_shadow_events
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar
  AND reason      = @reason::varchar;
