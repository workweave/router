// Package apm wires the OpenTelemetry SDK against an OTLP/gRPC collector so
// HTTP spans and Go runtime metrics show up in the shared SigNoz instance
// alongside the rest of Weave's services. It's independent of the router's
// own custom OTLP/HTTP emitter (internal/observability/otel), which keeps
// publishing high-cardinality per-decision spans separately.
//
// Enable via WV_APM_OTLP_ENDPOINT (SigNoz OTLP/gRPC host:port); unset = no-op.
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
	"workweave/router/internal/version"
)

// serviceName is the resource attribute the SigNoz UI groups spans + metrics
// by. Matches the convention from backend/internal/app/telemetry/otel.go.
const serviceName = "router"

var (
	initOnce       sync.Once
	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
)

// Init wires the OTel SDK against the SigNoz collector using
// WV_APM_OTLP_ENDPOINT (host:port for OTLP/gRPC). Empty = no-op. Idempotent.
// Mirrors backend/internal/app/telemetry/otel.go's init() so the router
// reports the same resource attribute shape as the rest of Weave.
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

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(version.Commit),
			semconv.DeploymentEnvironment(deployEnv),
		),
		resource.WithHost(),
		resource.WithContainer(),
	)
	if err != nil {
		log.Error("APM init: resource construction failed", "err", err)
		return
	}

	// TLS by default: some deployments run the router outside the SigNoz
	// collector's cluster, and plaintext gRPC would leak span attributes
	// (api_key_id, model names, decision reasons) on-path. Opt into insecure
	// transport explicitly via WV_APM_OTLP_INSECURE=true on trusted networks.
	insecure := config.GetOr("WV_APM_OTLP_INSECURE", "false") == "true"

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

	// Hooks runtime/metrics (goroutines, heap, GC, cgo calls) into the
	// MeterProvider just set above — without it SigNoz gets no metrics at all.
	if err := otelruntime.Start(otelruntime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		log.Warn("APM init: runtime instrumentation failed; SDK traces still active", "err", err)
	}

	log.Info("APM enabled",
		"endpoint", endpoint,
		"service", serviceName,
		"deployment_env", deployEnv,
		"version", version.Commit,
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

// Middleware wraps each request in an HTTP server span tagged with
// method/route/status, excluding health probes and /validate (infra polling would
// swamp trace volume). No-op when Init was never called.
func Middleware() gin.HandlerFunc {
	return otelgin.Middleware(
		serviceName,
		otelgin.WithGinFilter(func(c *gin.Context) bool {
			path := c.FullPath()
			return path != "/health" && path != "/readyz" && path != "/validate"
		}),
	)
}

// ShutdownWithContext flushes the trace + metric pipelines under the
// caller's deadline.
func ShutdownWithContext(ctx context.Context) {
	if tracerProvider != nil {
		_ = tracerProvider.Shutdown(ctx)
	}
	if meterProvider != nil {
		_ = meterProvider.Shutdown(ctx)
	}
}
