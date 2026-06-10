-- Records one cyclic-loop detection. Written at detection time with
-- context.Background() (the request ctx may already be canceled); the
-- post-escalation outcome is joined offline by session_key against
-- model_router_request_telemetry / session results.
-- name: InsertLoopEscalationEvent :exec
INSERT INTO router.loop_escalation_events (
    installation_id,
    session_key,
    role,
    looping_model,
    action,
    escalation_target,
    loop_tool,
    loop_input_hash,
    repeat_count,
    distinct_ratio,
    window_size
) VALUES (
    @installation_id::uuid,
    @session_key::bytea,
    @role::varchar,
    @looping_model::varchar,
    @action::varchar,
    @escalation_target::varchar,
    @loop_tool::varchar,
    @loop_input_hash::varchar,
    @repeat_count::int,
    @distinct_ratio::double precision,
    @window_size::int
);

-- Once-per-session budget check: any prior event for this (session, role)
-- means the detector already fired and must not fire again, even after the
-- session pin's TTL expires.
-- name: CountLoopEscalationEvents :one
SELECT COUNT(*)
FROM router.loop_escalation_events
WHERE session_key = @session_key::bytea
  AND role        = @role::varchar;
