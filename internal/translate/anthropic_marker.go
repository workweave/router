package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AnthropicRoutingMarkerWriter wraps an http.ResponseWriter and injects a
// routing-marker text block at index 0 of an Anthropic-format SSE stream.
// Upstream content_block_* indices are shifted by +1 to accommodate the
// injected block. Non-streaming responses pass through unmodified.
type AnthropicRoutingMarkerWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	marker string
	model  string

	buf bytes.Buffer

	streaming      bool
	headersEmitted bool
	markerEmitted  bool
}

// NewAnthropicRoutingMarkerWriter creates a writer that injects marker as a
// standalone text block at index 0 before any upstream data. If marker is
// empty, all writes pass through unchanged.
func NewAnthropicRoutingMarkerWriter(w http.ResponseWriter, model, marker string) *AnthropicRoutingMarkerWriter {
	flusher, _ := w.(http.Flusher)
	return &AnthropicRoutingMarkerWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		marker:  marker,
		model:   model,
	}
}

func (w *AnthropicRoutingMarkerWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *AnthropicRoutingMarkerWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *AnthropicRoutingMarkerWriter) Write(data []byte) (int, error) {
	if w.streaming && !w.markerEmitted {
		w.markerEmitted = true
		if w.marker != "" {
			if err := w.emitPreludeEvents(); err != nil {
				return 0, err
			}
		}
	}
	if !w.streaming || w.marker == "" {
		// Non-streaming or empty marker: fully transparent passthrough.
		return w.inner.Write(data)
	}
	// Streaming with a configured marker: parse upstream SSE, drop message_start,
	// shift content_block_* indices, pass everything else through.
	n, err := w.processUpstream(data)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Prelude commits headers and emits the routing marker immediately, before the
// upstream provider has returned a single byte. Call this right after the
// routing decision is made when the client requested streaming (streaming=true)
// so first-byte latency drops to ~routing time rather than upstream prefill +
// first decode. Safe to call once; subsequent Write/WriteHeader calls are
// idempotent. No-op when streaming is false or marker is empty.
func (w *AnthropicRoutingMarkerWriter) Prelude(streaming bool) error {
	if !streaming || w.markerEmitted {
		return nil
	}
	w.inner.Header().Set("Content-Type", "text/event-stream")
	w.streaming = true
	if !w.headersEmitted {
		w.headersEmitted = true
		w.inner.WriteHeader(http.StatusOK)
	}
	w.markerEmitted = true
	if w.marker == "" {
		w.bw.WriteString(": routing complete\n\n")
		if err := w.bw.Flush(); err != nil {
			return err
		}
		if w.flusher != nil {
			w.flusher.Flush()
		}
		return nil
	}
	return w.emitPreludeEvents()
}

// Flush implements http.Flusher.
func (w *AnthropicRoutingMarkerWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// emitPreludeEvents writes message_start followed by the routing marker as a
// text content block at index 0.
func (w *AnthropicRoutingMarkerWriter) emitPreludeEvents() error {
	// message_start — mirrors the envelope shape from AnthropicSSETranslator.
	w.bw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":")
	sse.WriteJSONString(w.bw, generateAnthropicMsgID())
	w.bw.WriteString(",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":")
	sse.WriteJSONString(w.bw, w.model)
	w.bw.WriteString(",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n")

	// content_block_start at index 0 (text block).
	w.bw.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

	// content_block_delta (text_delta) at index 0.
	w.bw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":")
	sse.WriteJSONString(w.bw, w.marker)
	w.bw.WriteString("}}\n\n")

	// content_block_stop at index 0.
	w.bw.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

	if err := w.bw.Flush(); err != nil {
		return err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return nil
}

// processUpstream parses upstream SSE events, drops message_start, and shifts
// content_block_* indices by +1. Non-indexed events (message_delta,
// message_stop, ping, error) pass through unchanged.
func (w *AnthropicRoutingMarkerWriter) processUpstream(data []byte) (int, error) {
	// Accumulate into a persistent buffer so an SSE event split across two
	// Write calls is held until its terminating blank line arrives, rather
	// than being parsed as a truncated (and silently dropped) event.
	w.buf.Write(data)
	for {
		event, n := sse.SplitNext(w.buf.Bytes())
		if n == 0 {
			break
		}
		eventType, eventData := sse.ParseEvent(event)

		if len(eventType) == 0 && len(eventData) == 0 {
			// Comment or empty — pass through as-is.
			if _, err := w.inner.Write(event[:n]); err != nil {
				return 0, err
			}
			w.buf.Next(n)
			continue
		}

		switch string(eventType) {
		case "message_start":
			// Drop upstream's message_start; we already sent one.
			w.buf.Next(n)
			continue

		case "content_block_start", "content_block_delta", "content_block_stop":
			// Rewrite the index field: shift by +1.
			currentIdx := gjson.GetBytes(eventData, "index").Int()
			rewritten, err := sjson.SetBytes(eventData, "index", currentIdx+1)
			if err != nil {
				// Fall through: emit original event if rewrite fails.
				if _, err := w.inner.Write(event[:n]); err != nil {
					return 0, err
				}
				w.buf.Next(n)
				continue
			}
			w.bw.WriteString("event: ")
			w.bw.Write(eventType)
			w.bw.WriteString("\ndata: ")
			w.bw.Write(rewritten)
			w.bw.WriteString("\n\n")
			if err := w.bw.Flush(); err != nil {
				return 0, err
			}
			w.buf.Next(n)

		default:
			// message_delta, message_stop, ping, error, etc. — pass through untouched.
			if _, err := w.inner.Write(event[:n]); err != nil {
				return 0, err
			}
			w.buf.Next(n)
		}
	}
	return len(data), nil
}

var _ http.ResponseWriter = (*AnthropicRoutingMarkerWriter)(nil)
var _ http.Flusher = (*AnthropicRoutingMarkerWriter)(nil)
