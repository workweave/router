// Package apm wires the OpenTelemetry SDK against an OTLP/gRPC collector so
// HTTP request spans and Go runtime metrics show up in the shared SigNoz
// instance at apm.app.workweave.ai alongside the rest of the Weave services.
//
// This sits alongside the router's own custom OTLP/HTTP emitter (which keeps
// emitting the per-decision spans defined in internal/observability/otel).
// The two pipelines are independent and can both be enabled — the custom
// emitter publishes high-cardinality routing spans; this package publishes
// the standard gin HTTP spans + runtime metrics that the rest of Weave's
// services already publish in the same shape.
//
// Enable by setting WV_APM_OTLP_ENDPOINT to the SigNoz OTLP/gRPC host:port.
// Unset = no-op (same convention as the custom emitter on
// OTEL_EXPORTER_OTLP_ENDPOINT).
package apm

import (
	"context"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	otelgin "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"

	"workweave/router/internal/config"
	"workweave/router/internal/observability"
)

// serviceName is the resource attribute the SigNoz UI groups spans + metrics
// by. Matches the convention from backend/internal/app/telemetry/otel.go.
const serviceName = "router"

var (
	initOnce       sync.Once
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
)

// Init wires the OTel SDK against the SigNoz collector. Reads
// WV_APM_OTLP_ENDPOINT (host:port for OTLP/gRPC). Empty = no-op. Idempotent.
//
// Mirrors backend/internal/app/telemetry/otel.go's init() in shape so the
// router shows up in apm.app.workweave.ai with the same resource attributes
// as the rest of Weave.
func Init() {
	initOnce.Do(initLocked)
}

func initLocked() {
	log := observability.Get()

	endpoint := config.GetOr("WV_APM_OTLP_ENDPOINT", "")
	if endpoint == "" {
		log.Info("APM disabled: WV_APM_OTLP_ENDPOINT unset")
		return
	}

	ctx := context.Background()
	deployEnv := config.GetOr("ROUTER_DEPLOYMENT_ENV", config.GetOr("ENV", "dev"))
	version := config.GetOr("ROUTER_VERSION", "unknown")

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version),
			semconv.DeploymentEnvironment(deployEnv),
		),
		resource.WithHost(),
		resource.WithContainer(),
	)
	if err != nil {
		log.Error("APM init: resource construction failed", "err", err)
		return
	}

	// Insecure: this is the internal collector behind the LB, same convention
	// as backend/internal/app/telemetry/otel.go. WV_APM_OTLP_INSECURE=false
	// flips to TLS for external collectors during local testing against
	// apm.app.workweave.ai directly.
	insecure := config.GetOr("WV_APM_OTLP_INSECURE", "true") == "true"

	traceExporter, err := newTraceExporter(ctx, endpoint, insecure)
	if err != nil {
		log.Error("APM init: trace exporter failed", "err", err)
		return
	}
	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tracerProvider)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	metricExporter, err := otlpmetricgrpc.New(ctx, metricGRPCOpts(endpoint, insecure)...)
	if err != nil {
		log.Error("APM init: metric exporter failed", "err", err)
		return
	}
	meterProvider = sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter,
			sdkmetric.WithInterval(60*time.Second),
		)),
	)
	otel.SetMeterProvider(meterProvider)

	// Without this the MeterProvider has no instruments registered against
	// it and the only metric SigNoz receives is the empty resource heartbeat.
	// otelruntime.Start hooks runtime/metrics (goroutines, heap, GC pauses,
	// cgo calls) into the global MeterProvider just set above.
	if err := otelruntime.Start(otelruntime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		log.Warn("APM init: runtime instrumentation failed; SDK traces still active", "err", err)
	}

	log.Info("APM enabled",
		"endpoint", endpoint,
		"service", serviceName,
		"deployment_env", deployEnv,
		"version", version,
		"insecure", insecure,
	)
}

func newTraceExporter(ctx context.Context, endpoint string, insecure bool) (sdktrace.SpanExporter, error) {
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	return otlptrace.New(ctx, otlptracegrpc.NewClient(opts...))
}

func metricGRPCOpts(endpoint string, insecure bool) []otlpmetricgrpc.Option {
	opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(endpoint)}
	if insecure {
		opts = append(opts, otlpmetricgrpc.WithInsecure())
	}
	return opts
}

// Middleware returns a gin middleware that wraps each request in an HTTP
// server span tagged with method/route/status. Routes /health and /validate
// are excluded — they're polled by infra and would swamp the trace volume.
// No-op when Init was never called or the SDK is disabled.
func Middleware() gin.HandlerFunc {
	return otelgin.Middleware(
		serviceName,
		otelgin.WithGinFilter(func(c *gin.Context) bool {
			path := c.FullPath()
			return path != "/health" && path != "/validate"
		}),
	)
}

// Shutdown flushes the trace + metric pipelines with a 5s default budget.
// Safe to call multiple times and safe when Init was never called.
//
// Most callers should prefer ShutdownWithContext so the parent process can
// budget the flush against its own SIGTERM window.
func Shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ShutdownWithContext(ctx)
}

// ShutdownWithContext flushes the trace + metric pipelines under the caller's
// deadline. Use this from a process-level shutdown handler that's already
// budgeting its remaining SIGTERM time across multiple flush stages.
func ShutdownWithContext(ctx context.Context) {
	if tracerProvider != nil {
		_ = tracerProvider.Shutdown(ctx)
	}
	if meterProvider != nil {
		_ = meterProvider.Shutdown(ctx)
	}
}
