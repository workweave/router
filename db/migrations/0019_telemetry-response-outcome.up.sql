BEGIN;

-- Add per-response outcome columns so routing-health queries can see finish
-- reason, tool-call counts, failover usage, and degenerate-response shadow
-- events without parsing log lines.  All columns nullable: OpenAI-path and
-- Gemini rows leave them NULL; old rows are unaffected.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN upstream_finish_reason TEXT,
    ADD COLUMN stop_reason            TEXT,
    ADD COLUMN tool_use_blocks        INT,
    ADD COLUMN invalid_tool_args_blocks INT,
    ADD COLUMN failover_used          BOOLEAN,
    ADD COLUMN degenerate_shadow      BOOLEAN;

-- Canonical production-traffic view.  Filters out eval/knob-sweep traffic
-- tagged via X-App: weave-eval* so routing-health queries don't need to
-- re-derive the predicate ad hoc.  For historical rows (before eval harnesses
-- sent the weave-eval prefix) additionally filter session_id IS NOT NULL.
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
