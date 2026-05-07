package otel

import (
	"bytes"
	"net/http"

	"github.com/tidwall/gjson"

	"workweave/router/internal/sse"
)

// providerAnthropic / providerOpenAI / providerGoogle are local mirrors of the
// providers.Provider* constants. This package is a leaf utility (must not
// import internal/providers), so we duplicate the short strings rather than
// introduce a circular import. Keep in sync with internal/providers/provider.go.
const (
	providerAnthropic = "anthropic"
	providerOpenAI    = "openai"
	providerGoogle    = "google"
)

// UsageSink receives extracted token usage. Translators call RecordUsage
// when they encounter usage data in already-parsed events, eliminating
// the need for a separate parse pass.
type UsageSink interface {
	RecordUsage(inputTokens, outputTokens int)
}

var (
	_ http.ResponseWriter = (*UsageExtractor)(nil)
	_ http.Flusher        = (*UsageExtractor)(nil)
	_ UsageSink           = (*UsageExtractor)(nil)
)

// UsageExtractor wraps an http.ResponseWriter and sniffs token usage
// from upstream responses (SSE and JSON) as bytes flow through.
// Only the unconsumed tail of the most recent Write is retained;
// complete SSE events are parsed and discarded immediately.
type UsageExtractor struct {
	inner    http.ResponseWriter
	provider string

	input  int
	output int

	leftover []byte
}

// NewUsageExtractor creates a usage-extracting writer. Provider determines
// the response format to parse. When inner is nil the extractor operates in
// sink-only mode: only RecordUsage and Tokens are valid; the ResponseWriter
// methods must not be called.
func NewUsageExtractor(inner http.ResponseWriter, provider string) *UsageExtractor {
	return &UsageExtractor{
		inner:    inner,
		provider: provider,
	}
}

func (u *UsageExtractor) Header() http.Header {
	return u.inner.Header()
}

func (u *UsageExtractor) WriteHeader(statusCode int) {
	u.inner.WriteHeader(statusCode)
}

// Write sniffs p for token usage data then delegates to the inner writer.
func (u *UsageExtractor) Write(p []byte) (int, error) {
	u.leftover = append(u.leftover, p...)
	u.scanBuffer()
	return u.inner.Write(p)
}

// Flush delegates to the inner writer's http.Flusher if present.
func (u *UsageExtractor) Flush() {
	if u.inner == nil {
		return
	}
	if f, ok := u.inner.(http.Flusher); ok {
		f.Flush()
	}
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

// Tokens returns the extracted input and output token counts. Nil receiver
// returns (0, 0) so callers can skip the extractor when OTel is disabled.
func (u *UsageExtractor) Tokens() (input, output int) {
	if u == nil {
		return 0, 0
	}
	return u.input, u.output
}

// scanBuffer splits buffered data on SSE event boundaries and extracts
// token usage from each complete event using zero-alloc gjson probes.
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
	case providerAnthropic:
		u.extractAnthropicSSE(eventType, data)
	case providerOpenAI, providerGoogle:
		u.extractOpenAISSE(data)
	}
}

// message_start carries input_tokens; message_delta carries output_tokens.
func (u *UsageExtractor) extractAnthropicSSE(eventType []byte, data []byte) {
	if !bytes.Equal(eventType, []byte("message_start")) && !bytes.Equal(eventType, []byte("message_delta")) {
		return
	}

	input, output, found := extractUsageGJSON(data, providerAnthropic)
	if !found {
		return
	}

	if bytes.Equal(eventType, []byte("message_start")) && input > 0 {
		u.input = input
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

	input, output, found := extractUsageGJSON(trimmed, u.provider)
	if !found {
		return
	}

	u.input = input
	u.output = output
}

func (u *UsageExtractor) tryExtractFromJSON() {
	if len(u.leftover) == 0 {
		return
	}

	input, output, found := extractUsageGJSON(u.leftover, u.provider)
	if !found {
		return
	}

	if input > 0 {
		u.input = input
	}
	if output > 0 {
		u.output = output
	}
}

// extractUsageGJSON probes for token usage fields using gjson, avoiding
// json.Unmarshal and map[string]any allocations entirely.
func extractUsageGJSON(data []byte, provider string) (input, output int, found bool) {
	usage := gjson.GetBytes(data, "usage")
	if !usage.Exists() && provider == providerAnthropic {
		usage = gjson.GetBytes(data, "message.usage")
	}
	if !usage.Exists() {
		return 0, 0, false
	}

	switch provider {
	case providerAnthropic:
		input = int(usage.Get("input_tokens").Int())
		output = int(usage.Get("output_tokens").Int())
	case providerOpenAI, providerGoogle:
		input = int(usage.Get("prompt_tokens").Int())
		output = int(usage.Get("completion_tokens").Int())
	default:
		return 0, 0, false
	}

	return input, output, true
}
