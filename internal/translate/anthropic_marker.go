package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// AnthropicRoutingMarkerWriter injects a routing-marker text block at index 0
// of an Anthropic SSE stream, shifting upstream content_block_* indices by +1.
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

	onOutputProgress func()
}

// NewAnthropicRoutingMarkerWriter injects marker as a text block at index 0.
// If marker is empty, writes pass through unchanged.
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

// Prelude commits headers and emits the marker before upstream responds, so
// first-byte latency is ~routing time instead of upstream prefill+decode. Call
// once right after the routing decision; later Write/WriteHeader calls are
// idempotent. No-op if streaming is false or marker is empty.
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
// content_block_* indices by +1; other events pass through unchanged.
func (w *AnthropicRoutingMarkerWriter) processUpstream(data []byte) (int, error) {
	// Buffer across Write calls so an SSE event split mid-stream isn't parsed
	// as truncated (and silently dropped) before its terminating blank line.
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
			// Mark on output-bearing content_block_delta only; start/stop are structural.
			if string(eventType) == "content_block_delta" && w.onOutputProgress != nil {
				w.onOutputProgress()
			}
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

// ArmOutputProgress fires mark on output-bearing content_block_delta frames only
// (not pings or structural frames) so the native output-stall watchdog tracks
// time-since-last-output. Returns false when not streaming or without a marker.
func (w *AnthropicRoutingMarkerWriter) ArmOutputProgress(mark func()) (armed bool) {
	if !w.streaming || w.marker == "" {
		return false
	}
	w.onOutputProgress = mark
	return true
}

var _ http.ResponseWriter = (*AnthropicRoutingMarkerWriter)(nil)
var _ http.Flusher = (*AnthropicRoutingMarkerWriter)(nil)
var _ providers.OutputProgressArmer = (*AnthropicRoutingMarkerWriter)(nil)
