package otel

import (
	"bytes"
	"net/http"

	"github.com/tidwall/gjson"

	"workweave/router/internal/providers"
	"workweave/router/internal/sse"
)

// UsageSink receives extracted token usage. Translators call it directly when
// they've already parsed usage from an event, skipping a separate parse pass.
type UsageSink interface {
	RecordUsage(inputTokens, outputTokens int)
	RecordCacheUsage(cacheCreationTokens, cacheReadTokens int)
}

var (
	_ http.ResponseWriter = (*UsageExtractor)(nil)
	_ http.Flusher        = (*UsageExtractor)(nil)
	_ UsageSink           = (*UsageExtractor)(nil)
)

// UsageExtractor wraps an http.ResponseWriter and sniffs token usage (SSE or
// JSON) as bytes flow through. Only the unconsumed tail is retained between writes.
type UsageExtractor struct {
	inner    http.ResponseWriter
	provider string

	input         int
	output        int
	cacheCreation int
	cacheRead     int

	leftover []byte
}

// NewUsageExtractor creates a usage-extracting writer for the given provider's
// response format. If inner is nil, only RecordUsage/Tokens are valid — the
// ResponseWriter methods must not be called.
func NewUsageExtractor(inner http.ResponseWriter, provider string) *UsageExtractor {
	return &UsageExtractor{
		inner:    inner,
		provider: provider,
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
	if inputTokens > 0 {
		u.input = inputTokens
	}
	if outputTokens > 0 {
		u.output = outputTokens
	}
}

// RecordCacheUsage sets cache token counts directly. OpenAI has no cache-creation
// concept, so callers pass 0 for cacheCreationTokens.
func (u *UsageExtractor) RecordCacheUsage(cacheCreationTokens, cacheReadTokens int) {
	if cacheCreationTokens > 0 {
		u.cacheCreation = cacheCreationTokens
	}
	if cacheReadTokens > 0 {
		u.cacheRead = cacheReadTokens
	}
}

// Tokens returns the extracted input and output token counts.
func (u *UsageExtractor) Tokens() (input, output int) {
	if u == nil {
		return 0, 0
	}
	return u.input, u.output
}

// CacheTokens returns the extracted cache creation/read counts. Zero means the
// provider doesn't emit cache tokens (Google) or there were no cache hits.
func (u *UsageExtractor) CacheTokens() (creation, read int) {
	if u == nil {
		return 0, 0
	}
	return u.cacheCreation, u.cacheRead
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
	switch u.provider {
	case providers.ProviderAnthropic:
		u.extractAnthropicSSE(eventType, data)
	case providers.ProviderOpenAI, providers.ProviderGoogle:
		u.extractOpenAISSE(data)
	}
}

// message_start carries input_tokens + cache tokens; message_delta carries output_tokens.
func (u *UsageExtractor) extractAnthropicSSE(eventType []byte, data []byte) {
	if !bytes.Equal(eventType, []byte("message_start")) && !bytes.Equal(eventType, []byte("message_delta")) {
		return
	}

	input, output, cacheCreation, cacheRead, found := extractUsageGJSON(data, providers.ProviderAnthropic)
	if !found {
		return
	}

	if bytes.Equal(eventType, []byte("message_start")) {
		if input > 0 {
			u.input = input
		}
		if cacheCreation > 0 {
			u.cacheCreation = cacheCreation
		}
		if cacheRead > 0 {
			u.cacheRead = cacheRead
		}
	}
	if bytes.Equal(eventType, []byte("message_delta")) && output > 0 {
		u.output = output
	}
}

// Final chunk with stream_options.include_usage=true carries the counts.
func (u *UsageExtractor) extractOpenAISSE(data []byte) {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("[DONE]")) {
		return
	}

	input, output, cacheCreation, cacheRead, found := extractUsageGJSON(trimmed, u.provider)
	if !found {
		return
	}

	u.input = input
	u.output = output
	if cacheCreation > 0 {
		u.cacheCreation = cacheCreation
	}
	if cacheRead > 0 {
		u.cacheRead = cacheRead
	}
}

func (u *UsageExtractor) tryExtractFromJSON() {
	if len(u.leftover) == 0 {
		return
	}

	input, output, cacheCreation, cacheRead, found := extractUsageGJSON(u.leftover, u.provider)
	if !found {
		return
	}

	if input > 0 {
		u.input = input
	}
	if output > 0 {
		u.output = output
	}
	if cacheCreation > 0 {
		u.cacheCreation = cacheCreation
	}
	if cacheRead > 0 {
		u.cacheRead = cacheRead
	}
}

// extractUsageGJSON probes usage fields via gjson (no json.Unmarshal/map allocs).
// OpenAI has no cache-creation field; cacheRead maps from cached_tokens.
// Google's native :generateContent uses usageMetadata; its OpenAI-compat surface
// uses the OpenAI shape instead.
func extractUsageGJSON(data []byte, provider string) (input, output, cacheCreation, cacheRead int, found bool) {
	if provider == providers.ProviderGoogle {
		if meta := gjson.GetBytes(data, "usageMetadata"); meta.Exists() {
			input = int(meta.Get("promptTokenCount").Int())
			output = int(meta.Get("candidatesTokenCount").Int())
			cacheRead = int(meta.Get("cachedContentTokenCount").Int())
			return input, output, 0, cacheRead, true
		}
	}

	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() && provider == providers.ProviderAnthropic {
		usage = gjson.GetBytes(data, "message.usage")
	}
	// OpenAI Responses streaming nests usage under the terminal response event
	// (response.completed); the non-streaming body carries it at the top level.
	if !usage.Exists() && provider == providers.ProviderOpenAI {
		usage = gjson.GetBytes(data, "response.usage")
	}
	if !usage.Exists() {
		return 0, 0, 0, 0, false
	}

	switch provider {
	case providers.ProviderAnthropic:
		input = int(usage.Get("input_tokens").Int())
		output = int(usage.Get("output_tokens").Int())
		cacheCreation = int(usage.Get("cache_creation_input_tokens").Int())
		cacheRead = int(usage.Get("cache_read_input_tokens").Int())
	case providers.ProviderOpenAI, providers.ProviderGoogle:
		// Chat Completions uses prompt_tokens/completion_tokens; Responses API
		// (Codex passthrough) uses input_tokens/output_tokens. Probe both.
		if pt := usage.Get("prompt_tokens"); pt.Exists() {
			input = int(pt.Int())
			output = int(usage.Get("completion_tokens").Int())
			cacheRead = int(usage.Get("prompt_tokens_details.cached_tokens").Int())
		} else {
			input = int(usage.Get("input_tokens").Int())
			output = int(usage.Get("output_tokens").Int())
			cacheRead = int(usage.Get("input_tokens_details.cached_tokens").Int())
		}
	default:
		return 0, 0, 0, 0, false
	}

	return input, output, cacheCreation, cacheRead, true
}
