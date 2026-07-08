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
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	otelgin "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
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
	// promHandler serves the OTel metrics registered by this SDK over the
	// Prometheus text format. Set during init when the Prometheus reader is
	// wired; nil until then, guarded by PrometheusHandler.
	promHandler http.Handler
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

	// Metric readers are additive: the Prometheus reader is always wired so
	// /metrics is scrapeable in every deployment, and the OTLP push reader is
	// added only when a SigNoz collector endpoint is configured.
	readers := []sdkmetric.Reader{}

	// The Prometheus exporter is itself an sdkmetric.Reader that renders the
	// collected metrics on demand via promhttp. It registers with the default
	// Prometheus registry, which promhttp.Handler() serves.
	promReader, err := promexporter.New()
	if err != nil {
		log.Error("APM init: prometheus exporter failed", "err", err)
	} else {
		readers = append(readers, promReader)
		promHandler = promhttp.Handler()
	}

	endpoint := config.GetOr("WV_APM_OTLP_ENDPOINT", "")
	// TLS by default. The router runs outside the cluster the SigNoz
	// collector is on for some deployments, so silently shipping spans over
	// plaintext gRPC would leak whatever's in attributes (api_key_id, model
	// names, decision reasons) to anyone on-path. Operators opt into insecure
	// transport explicitly via WV_APM_OTLP_INSECURE=true when the collector
	// is reachable only over a trusted internal network.
	insecure := config.GetOr("WV_APM_OTLP_INSECURE", "false") == "true"

	if endpoint != "" {
		traceExporter, terr := newTraceExporter(ctx, endpoint, insecure)
		if terr != nil {
			log.Error("APM init: trace exporter failed", "err", terr)
		} else {
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
		}

		metricExporter, merr := otlpmetricgrpc.New(ctx, metricGRPCOpts(endpoint, insecure)...)
		if merr != nil {
			log.Error("APM init: metric exporter failed", "err", merr)
		} else {
			readers = append(readers, sdkmetric.NewPeriodicReader(metricExporter,
				sdkmetric.WithInterval(60*time.Second),
			))
		}
	}

	if len(readers) == 0 {
		log.Info("APM disabled: no metric readers wired")
		return
	}

	opts := []sdkmetric.Option{sdkmetric.WithResource(res)}
	for _, r := range readers {
		opts = append(opts, sdkmetric.WithReader(r))
	}
	meterProvider = sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(meterProvider)

	// Without this the MeterProvider has no instruments registered against
	// it and the only metric exported is the empty resource heartbeat.
	// otelruntime.Start hooks runtime/metrics (goroutines, heap, GC pauses,
	// cgo calls) into the global MeterProvider just set above.
	if err := otelruntime.Start(otelruntime.WithMinimumReadMemStatsInterval(15 * time.Second)); err != nil {
		log.Warn("APM init: runtime instrumentation failed", "err", err)
	}

	log.Info("APM enabled",
		"otlp_endpoint", endpoint,
		"prometheus", promHandler != nil,
		"service", serviceName,
		"deployment_env", deployEnv,
		"version", version.Commit,
		"insecure", insecure,
	)
}

// PrometheusHandler returns an http.Handler that serves the OTel metrics
// registered by this SDK in the Prometheus text format. When Init was never
// called or the exporter failed to wire, it returns a handler that replies 503
// so mounting the route is always safe.
func PrometheusHandler() http.Handler {
	if promHandler == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "prometheus metrics unavailable", http.StatusServiceUnavailable)
		})
	}
	return promHandler
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
