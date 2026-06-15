BEGIN;

-- Shadow-mode instrumentation for the right-sizing study's tier-cap lever
-- (action item #2, tool_result half). ~90% of turns are tool_result
-- continuations that short-circuit to the session pin verbatim -- no scorer
-- runs, so there is no score vector to reason about. The cheap structural
-- triviality signal for those turns is the INCOMING tool-output size: a tiny
-- tool_result almost always precedes a trivial continuation that a cheaper
-- tier could serve, while a large one often precedes heavy work.
--
-- tool_result_bytes records that size (summed raw-JSON bytes of the trailing
-- turn's tool_result payloads) so the tier-cap opportunity is measurable
-- offline (size distribution x served model x output_tokens) before any cap is
-- armed. Pure telemetry: no routing behavior changes. NULL when the turn
-- carries no trailing tool_result.
ALTER TABLE router.model_router_request_telemetry
    ADD COLUMN tool_result_bytes INT;

COMMENT ON COLUMN router.model_router_request_telemetry.tool_result_bytes IS
    'Summed raw-JSON byte size of the trailing turn''s tool_result payload(s) -- the incoming tool-output size. Structural triviality proxy for the tier-cap shadow. NULL when the turn carries no trailing tool_result.';

-- Recreate the production-traffic view so the new column surfaces through it
-- (CREATE VIEW ... SELECT * freezes its column list at creation). Body
-- unchanged from migration 0019/0021/0022.
DROP VIEW router.production_request_telemetry;
CREATE VIEW router.production_request_telemetry AS
SELECT * FROM router.model_router_request_telemetry
WHERE span_type = 'router.upstream'
  AND (client_app IS NULL OR client_app NOT LIKE 'weave-eval%');

COMMIT;
