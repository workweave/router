package otel

import (
	"bytes"
	"net/http"

	"github.com/tidwall/gjson"

	"workweave/router/internal/providers"
	"workweave/router/internal/sse"
	"workweave/router/internal/translate"
)

// UsageSink receives extracted token usage. Translators call it directly when
// they've already parsed usage from an event, skipping a separate parse pass.
type UsageSink = translate.UsageSink

var (
	_ http.ResponseWriter = (*UsageExtractor)(nil)
	_ http.Flusher        = (*UsageExtractor)(nil)
	_ UsageSink           = (*UsageExtractor)(nil)
)

// UsageExtractor wraps an http.ResponseWriter and sniffs token usage (SSE or
// JSON) as bytes flow through. Only the unconsumed tail is retained between writes.
type UsageExtractor struct {
	inner  http.ResponseWriter
	source providers.UsageSource
	usage  translate.UsageReducer

	leftover []byte
}

// NewUsageExtractor creates a usage-extracting writer for the given upstream
// wire source. If inner is nil, only RecordUsage/Tokens are valid — the
// ResponseWriter methods must not be called.
func NewUsageExtractor(inner http.ResponseWriter, source providers.UsageSource) *UsageExtractor {
	return &UsageExtractor{
		inner:  inner,
		source: source,
	}
}

func (u *UsageExtractor) Header() http.Header {
	if u.inner == nil {
		return nil
	}
	return u.inner.Header()
}

func (u *UsageExtractor) WriteHeader(statusCode int) {
	if u.inner == nil {
		return
	}
	u.inner.WriteHeader(statusCode)
}

// Write sniffs p for token usage data then delegates to the inner writer.
func (u *UsageExtractor) Write(p []byte) (int, error) {
	if u.inner == nil {
		return len(p), nil
	}
	u.leftover = append(u.leftover, p...)
	u.scanBuffer()
	return u.inner.Write(p)
}

func (u *UsageExtractor) Flush() {
	if u.inner == nil {
		return
	}
	if f, ok := u.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// ArmOutputProgress forwards the watchdog arm call to inner if it supports it.
// UsageExtractor can't itself distinguish an output frame from a keepalive, so
// this must pass through or wrapping would hide the hook from the provider client.
func (u *UsageExtractor) ArmOutputProgress(mark func()) (armed bool) {
	if u.inner == nil {
		return false
	}
	arm, ok := u.inner.(interface{ ArmOutputProgress(func()) bool })
	if !ok {
		return false
	}
	return arm.ArmOutputProgress(mark)
}

// RecordUsage sets token counts directly, bypassing SSE parsing. Called by
// translators that have already parsed usage from the upstream event stream.
func (u *UsageExtractor) RecordUsage(inputTokens, outputTokens int) {
	u.RecordUsageObservation(translate.UsageObservation{
		Phase: translate.UsagePhaseTerminal,
		Values: translate.UsageValues{
			InputTokens:  positiveUsageInt(inputTokens),
			OutputTokens: positiveUsageInt(outputTokens),
		},
	})
}

// RecordUsageValues records a complete usage report while retaining explicit
// zero counters as distinct from fields omitted by incremental translators.
func (u *UsageExtractor) RecordUsageValues(values translate.UsageValues) {
	u.RecordUsageObservation(translate.UsageObservation{
		Phase:  translate.UsagePhaseTerminal,
		Values: values,
	})
}

// RecordUsageObservation records usage with its source-stream phase preserved.
func (u *UsageExtractor) RecordUsageObservation(observation translate.UsageObservation) {
	u.usage.Observe(observation)
}

// RecordCacheUsage sets cache token counts directly. OpenAI has no cache-creation
// concept, so callers pass 0 for cacheCreationTokens.
func (u *UsageExtractor) RecordCacheUsage(cacheCreationTokens, cacheReadTokens int) {
	u.usage.Observe(translate.UsageObservation{
		Phase: translate.UsagePhaseTerminal,
		Values: translate.UsageValues{
			CacheCreationInputTokens: positiveUsageInt(cacheCreationTokens),
			CacheReadInputTokens:     positiveUsageInt(cacheReadTokens),
		},
	})
}

// Tokens returns the extracted input and output token counts.
func (u *UsageExtractor) Tokens() (input, output int) {
	snapshot := u.Usage()
	return usageValue(snapshot.InputTokens), usageValue(snapshot.OutputTokens)
}

// CacheTokens returns the extracted cache creation/read counts. Zero means the
// provider doesn't emit cache tokens (Google) or there were no cache hits.
func (u *UsageExtractor) CacheTokens() (creation, read int) {
	snapshot := u.Usage()
	return usageValue(snapshot.CacheCreationInputTokens), usageValue(snapshot.CacheReadInputTokens)
}

// Usage returns the presence-aware, provider-authoritative usage state.
func (u *UsageExtractor) Usage() translate.UsageSnapshot {
	if u == nil {
		return translate.UsageSnapshot{Authority: translate.UsageAuthorityMissing}
	}
	return u.usage.Snapshot()
}

// scanBuffer splits buffered data on SSE event boundaries and extracts token
// usage from each complete event using zero-alloc gjson probes.
func (u *UsageExtractor) scanBuffer() {
	data := u.leftover

	for {
		event, n := sse.SplitNext(data)
		if n == 0 {
			break
		}
		data = data[n:]

		eventType, payload := sse.ParseEvent(event)
		if len(payload) == 0 {
			continue
		}

		u.extractFromSSEEvent(eventType, payload)
	}

	n := copy(u.leftover, data)
	u.leftover = u.leftover[:n]

	u.tryExtractFromJSON()
}

func (u *UsageExtractor) extractFromSSEEvent(eventType []byte, data []byte) {
	switch u.source.Family {
	case providers.FamilyAnthropic:
		u.extractAnthropicSSE(eventType, data)
	case providers.FamilyOpenAICompat, providers.FamilyGemini:
		u.extractOpenAISSE(eventType, data)
	}
}

// message_start carries input_tokens + cache tokens; message_delta carries output_tokens.
func (u *UsageExtractor) extractAnthropicSSE(eventType []byte, data []byte) {
	if !bytes.Equal(eventType, []byte("message_start")) && !bytes.Equal(eventType, []byte("message_delta")) {
		return
	}

	values, found := extractUsageGJSON(data, u.source)
	if !found {
		return
	}

	if bytes.Equal(eventType, []byte("message_start")) {
		u.usage.Observe(translate.UsageObservation{Phase: translate.UsagePhaseStart, Values: values, Placeholder: true})
	}
	if bytes.Equal(eventType, []byte("message_delta")) {
		u.usage.Observe(translate.UsageObservation{Phase: translate.UsagePhaseTerminal, Values: values})
	}
}

// Final chunk with stream_options.include_usage=true carries the counts.
func (u *UsageExtractor) extractOpenAISSE(eventType []byte, data []byte) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return
	}

	values, found := extractUsageGJSON(trimmed, u.source)
	if !found {
		return
	}
	phase := translate.UsagePhaseTerminal
	if bytes.Equal(eventType, []byte("response.created")) {
		phase = translate.UsagePhaseStart
	}
	u.usage.Observe(translate.UsageObservation{Phase: phase, Values: values, Placeholder: phase == translate.UsagePhaseStart})
}

func (u *UsageExtractor) tryExtractFromJSON() {
	if len(u.leftover) == 0 {
		return
	}

	values, found := extractUsageGJSON(u.leftover, u.source)
	if !found {
		return
	}
	u.usage.Observe(translate.UsageObservation{Phase: translate.UsagePhaseTerminal, Values: values})
}

// extractUsageGJSON probes usage fields via gjson (no json.Unmarshal/map allocs).
// OpenAI has no cache-creation field; cacheRead maps from cached_tokens.
// Google's native :generateContent uses usageMetadata; its OpenAI-compat surface
// uses the OpenAI shape instead.
func extractUsageGJSON(data []byte, source providers.UsageSource) (values translate.UsageValues, found bool) {
	if source.Family == providers.FamilyGemini {
		if meta := gjson.GetBytes(data, "usageMetadata"); meta.Exists() {
			return translate.UsageValues{
				InputTokens:          usageGJSONInt(meta.Get("promptTokenCount")),
				OutputTokens:         usageGJSONInt(meta.Get("candidatesTokenCount")),
				CacheReadInputTokens: usageGJSONInt(meta.Get("cachedContentTokenCount")),
				ReasoningTokens:      usageGJSONInt(meta.Get("thoughtsTokenCount")),
			}, true
		}
	}

	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() && source.Family == providers.FamilyAnthropic {
		usage = gjson.GetBytes(data, "message.usage")
	}
	// OpenAI Responses streaming nests usage under the terminal response event
	// (response.completed); the non-streaming body carries it at the top level.
	if !usage.Exists() && source.Family == providers.FamilyOpenAICompat {
		usage = gjson.GetBytes(data, "response.usage")
	}
	if !usage.Exists() {
		return translate.UsageValues{}, false
	}

	switch source.Family {
	case providers.FamilyAnthropic:
		return translate.UsageValues{
			InputTokens:              usageGJSONInt(usage.Get("input_tokens")),
			OutputTokens:             usageGJSONInt(usage.Get("output_tokens")),
			CacheCreationInputTokens: usageGJSONInt(usage.Get("cache_creation_input_tokens")),
			CacheReadInputTokens:     usageGJSONInt(usage.Get("cache_read_input_tokens")),
		}, true
	case providers.FamilyOpenAICompat, providers.FamilyGemini:
		// Chat Completions uses prompt_tokens/completion_tokens; Responses API
		// (Codex passthrough) uses input_tokens/output_tokens. Probe both.
		if pt := usage.Get("prompt_tokens"); pt.Exists() {
			return translate.UsageValues{
				InputTokens:              usageGJSONInt(pt),
				OutputTokens:             usageGJSONInt(usage.Get("completion_tokens")),
				CacheReadInputTokens:     usageGJSONInt(usage.Get("prompt_tokens_details.cached_tokens")),
				ReasoningTokens:          usageGJSONInt(usage.Get("completion_tokens_details.reasoning_tokens")),
				AudioInputTokens:         usageGJSONInt(usage.Get("prompt_tokens_details.audio_tokens")),
				AudioOutputTokens:        usageGJSONInt(usage.Get("completion_tokens_details.audio_tokens")),
				AcceptedPredictionTokens: usageGJSONInt(usage.Get("completion_tokens_details.accepted_prediction_tokens")),
				RejectedPredictionTokens: usageGJSONInt(usage.Get("completion_tokens_details.rejected_prediction_tokens")),
			}, true
		}
		return translate.UsageValues{
			InputTokens:              usageGJSONInt(usage.Get("input_tokens")),
			OutputTokens:             usageGJSONInt(usage.Get("output_tokens")),
			CacheReadInputTokens:     usageGJSONInt(usage.Get("input_tokens_details.cached_tokens")),
			ReasoningTokens:          usageGJSONInt(usage.Get("output_tokens_details.reasoning_tokens")),
			AudioInputTokens:         usageGJSONInt(usage.Get("input_tokens_details.audio_tokens")),
			AudioOutputTokens:        usageGJSONInt(usage.Get("output_tokens_details.audio_tokens")),
			AcceptedPredictionTokens: usageGJSONInt(usage.Get("output_tokens_details.accepted_prediction_tokens")),
			RejectedPredictionTokens: usageGJSONInt(usage.Get("output_tokens_details.rejected_prediction_tokens")),
		}, true
	default:
		return translate.UsageValues{}, false
	}
}

func usageGJSONInt(value gjson.Result) *int {
	if !value.Exists() {
		return nil
	}
	result := int(value.Int())
	return &result
}

func usageInt(value int) *int {
	return &value
}

func positiveUsageInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return usageInt(value)
}

func usageValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
