-- Records one /router-feedback submission. Written with
-- context.Background() (the synthetic ack response may already have been
-- flushed and the request ctx canceled). served_model is the session pin's
-- LastServedModel at submission time; empty when the session had no pin.
-- name: InsertRouterFeedback :exec
INSERT INTO router.router_feedback (
    installation_id,
    session_key,
    role,
    router_user_id,
    client_app,
    session_id,
    requested_model,
    served_model,
    feedback
) VALUES (
    @installation_id::uuid,
    @session_key::bytea,
    @role::varchar,
    sqlc.narg('router_user_id')::uuid,
    sqlc.narg('client_app')::text,
    sqlc.narg('session_id')::varchar,
    @requested_model::varchar,
    @served_model::varchar,
    @feedback::text
);
