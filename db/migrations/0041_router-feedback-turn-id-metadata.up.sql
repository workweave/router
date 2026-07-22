BEGIN;

ALTER TABLE router.router_feedback
    ADD COLUMN request_id VARCHAR,
    ADD COLUMN route_id VARCHAR;

COMMENT ON COLUMN router.router_feedback.request_id IS
    'Telemetry request_id for the specific turn this feedback targets. NULL when no sequence was specified.';
COMMENT ON COLUMN router.router_feedback.route_id IS
    'Sidecar correlation id from the telemetry row (HMM/RL). NULL when no sequence was specified.';

COMMIT;
