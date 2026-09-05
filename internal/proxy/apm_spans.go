package proxy

import (
	"context"

	"weave-os/router/internal/router"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const proxyTracerName = "weave-os/router/internal/proxy"

var proxyFlowTracer = otel.Tracer(proxyTracerName)

func startRoutingSpan(ctx context.Context, req router.Request) (context.Context, trace.Span) {
	return proxyFlowTracer.Start(ctx, "router.routing",
		trace.WithAttributes(
			attribute.String("requested.model", req.RequestedModel),
			attribute.Int("request.estimated_input_tokens", req.EstimatedInputTokens),
			attribute.Bool("request.has_tools", req.HasTools),
			attribute.String("router.strategy", string(router.StrategyFromContext(ctx))),
		),
	)
}

func finishRoutingSpan(span trace.Span, decision router.Decision, err error) {
	span.SetAttributes(
		attribute.String("decision.model", decision.Model),
		attribute.String("decision.provider", decision.Provider),
		attribute.String("decision.reason", decision.Reason),
	)
	finishFlowSpan(span, err)
}

func startInferenceSpan(ctx context.Context, decision router.Decision) (context.Context, trace.Span) {
	return proxyFlowTracer.Start(ctx, "router.inference",
		trace.WithAttributes(
			attribute.String("decision.model", decision.Model),
			attribute.String("decision.provider", decision.Provider),
			attribute.String("decision.reason", decision.Reason),
		),
	)
}

func finishInferenceSpan(span trace.Span, decision router.Decision, provider string, attempt int, err error) {
	span.SetAttributes(
		attribute.String("served.model", decision.Model),
		attribute.String("served.provider", provider),
	)
	if attempt >= 0 {
		span.SetAttributes(attribute.Int("upstream.binding_index", attempt))
	}
	if status := upstreamStatus(err); status != 0 {
		span.SetAttributes(attribute.Int("upstream.status_code", status))
	}
	finishFlowSpan(span, err)
}

func finishFlowSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

func restoreParentSpan(ctx, parent context.Context) context.Context {
	return trace.ContextWithSpan(ctx, trace.SpanFromContext(parent))
}
