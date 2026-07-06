package translate

// UsageSink receives extracted token usage. Translators call it directly when
// they've already parsed usage from an event, skipping a separate parse pass.
// Declared here (not in internal/observability/otel) because translate is an
// I/O-free inner-ring package and must not import the otel adapter; otel's
// UsageExtractor satisfies this interface structurally, with no otel-side
// changes needed since Go interfaces are structurally typed.
type UsageSink interface {
	RecordUsage(inputTokens, outputTokens int)
	RecordCacheUsage(cacheCreationTokens, cacheReadTokens int)
}
