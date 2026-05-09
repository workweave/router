BEGIN;

CREATE TABLE router.model_router_request_telemetry (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    installation_id           UUID NOT NULL REFERENCES router.model_router_installations(id) ON DELETE CASCADE,
    request_id                VARCHAR NOT NULL,
    span_type                 VARCHAR NOT NULL,
    trace_id                  VARCHAR NOT NULL,
    timestamp                 TIMESTAMPTZ NOT NULL,
    requested_model           VARCHAR,
    decision_model            VARCHAR,
    decision_provider         VARCHAR,
    decision_reason           VARCHAR,
    estimated_input_tokens    INT DEFAULT 0,
    sticky_hit                BOOLEAN DEFAULT FALSE,
    embed_input               VARCHAR,
    input_tokens              INT DEFAULT 0,
    output_tokens             INT DEFAULT 0,
    requested_input_cost_usd  NUMERIC(16, 6) DEFAULT 0,
    requested_output_cost_usd NUMERIC(16, 6) DEFAULT 0,
    actual_input_cost_usd     NUMERIC(16, 6) DEFAULT 0,
    actual_output_cost_usd    NUMERIC(16, 6) DEFAULT 0,
    route_latency_ms          BIGINT,
    upstream_latency_ms       BIGINT,
    total_latency_ms          BIGINT,
    cross_format              BOOLEAN DEFAULT FALSE,
    upstream_status_code      INT,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX ON router.model_router_request_telemetry (installation_id, timestamp DESC);
CREATE UNIQUE INDEX ON router.model_router_request_telemetry (installation_id, request_id, span_type);

COMMIT;
