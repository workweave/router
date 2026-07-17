package apm

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Translation metrics deliberately use only bounded protocol and policy
// dimensions. Provider, model, JSON path, and raw reason stay in request logs
// because they would create unbounded metric-cardinality in a shared collector.
var (
	translationMetricsOnce sync.Once
	compatibilityCounter   metric.Int64Counter
	transformCounter       metric.Int64Counter
)

func initTranslationMetrics() {
	meter := otel.Meter("workweave/router/translation")
	compatibilityCounter, _ = meter.Int64Counter(
		"router.translation.compatibility.exclusions",
		metric.WithDescription("Translation compatibility exclusions observed during routing."),
	)
	transformCounter, _ = meter.Int64Counter(
		"router.translation.transforms",
		metric.WithDescription("Translation transforms observed while adapting an inbound request."),
	)
}

// RecordTranslationCompatibility increments one counter per compatibility
// exclusion. requirement, source_format, target_family, mode, and enforced all
// come from fixed enums controlled by the router.
func RecordTranslationCompatibility(ctx context.Context, requirement, sourceFormat, targetFamily, mode string, enforced bool) {
	translationMetricsOnce.Do(initTranslationMetrics)
	if compatibilityCounter == nil {
		return
	}
	compatibilityCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("translation.requirement", requirement),
		attribute.String("translation.source_format", sourceFormat),
		attribute.String("translation.target_family", targetFamily),
		attribute.String("translation.mode", mode),
		attribute.Bool("translation.enforced", enforced),
	))
}

// RecordTranslationTransform increments one counter per explicit ingress
// transform. action and source_format are fixed values; the transform's path
// is intentionally excluded from the metric and remains available in logs.
func RecordTranslationTransform(ctx context.Context, code, action, sourceFormat, mode string) {
	translationMetricsOnce.Do(initTranslationMetrics)
	if transformCounter == nil {
		return
	}
	transformCounter.Add(ctx, 1, metric.WithAttributes(
		attribute.String("translation.code", code),
		attribute.String("translation.action", action),
		attribute.String("translation.source_format", sourceFormat),
		attribute.String("translation.mode", mode),
	))
}
