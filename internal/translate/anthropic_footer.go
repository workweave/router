package translate

import (
	"bufio"
	"bytes"
	"net/http"
	"strings"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// naturalStopReasons are the Anthropic stop_reason values that mark a turn ending
// with a real, user-facing answer. Footer injection is gated on these so the
// thumbs only appear on answer turns — not on intermediate tool_use turns (an
// agent's tool-call steps), truncated max_tokens turns, or refusals.
var naturalStopReasons = map[string]struct{}{
	"end_turn":      {},
	"stop_sequence": {},
}

// AnthropicRoutingFooterWriter wraps an http.ResponseWriter and appends a footer
// text content block at the END of an Anthropic-format SSE stream — after all
// upstream content blocks, immediately before message_delta — so any client that
// renders the answer also renders a post-answer affordance (a feedback thumb).
//
// It is the mirror of AnthropicRoutingMarkerWriter's index-0 prelude: it never
// shifts indices, it only observes the highest content_block index and emits one
// more block after it. Non-streaming responses and an empty footer pass through
// untouched, and the footer is injected only when the turn completes naturally
// (see naturalStopReasons) so tool-call turns stay clean.
type AnthropicRoutingFooterWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	footer string

	buf bytes.Buffer

	streaming      bool
	headersEmitted bool
	footerEmitted  bool
	// maxIndex is the highest content_block index seen so far; -1 until the
	// first content block arrives, which also gates injection on a non-empty
	// answer.
	maxIndex int
}

// NewAnthropicRoutingFooterWriter wraps w so that footer is appended as a
// trailing text block at the end of a streamed Anthropic response. If footer is
// empty, all writes pass through unchanged.
func NewAnthropicRoutingFooterWriter(w http.ResponseWriter, footer string) *AnthropicRoutingFooterWriter {
	flusher, _ := w.(http.Flusher)
	return &AnthropicRoutingFooterWriter{
		inner:    w,
		flusher:  flusher,
		bw:       bufio.NewWriterSize(w, 4096),
		footer:   footer,
		maxIndex: -1,
	}
}

func (w *AnthropicRoutingFooterWriter) Header() http.Header {
	return w.inner.Header()
}

func (w *AnthropicRoutingFooterWriter) WriteHeader(code int) {
	if w.headersEmitted {
		return
	}
	ct := w.inner.Header().Get("Content-Type")
	w.streaming = strings.Contains(ct, "text/event-stream") && code < 400
	w.headersEmitted = true
	w.inner.WriteHeader(code)
}

func (w *AnthropicRoutingFooterWriter) Write(data []byte) (int, error) {
	if !w.streaming || w.footer == "" {
		// Non-streaming or no footer: fully transparent passthrough.
		return w.inner.Write(data)
	}
	return w.processUpstream(data)
}

// Flush implements http.Flusher.
func (w *AnthropicRoutingFooterWriter) Flush() {
	if w.flusher != nil {
		w.flusher.Flush()
	}
}

// processUpstream parses the downstream Anthropic SSE, tracks the highest
// content_block index, and injects the footer block right before the first
// message_delta whose stop_reason is a natural turn end. Everything is forwarded
// in order; the only mutation is the inserted block.
func (w *AnthropicRoutingFooterWriter) processUpstream(data []byte) (int, error) {
	// Hold a partial trailing event until its terminating blank line arrives so
	// an event split across two Write calls is parsed whole, not truncated.
	w.buf.Write(data)
	for {
		event, n := sse.SplitNext(w.buf.Bytes())
		if n == 0 {
			break
		}
		eventType, eventData := sse.ParseEvent(event)

		switch string(eventType) {
		case "content_block_start", "content_block_delta", "content_block_stop":
			if idx := int(gjson.GetBytes(eventData, "index").Int()); idx > w.maxIndex {
				w.maxIndex = idx
			}
			w.bw.Write(event[:n])

		case "message_delta":
			if !w.footerEmitted && w.shouldInject(eventData) {
				w.emitFooterBlock(w.maxIndex + 1)
				w.footerEmitted = true
			}
			w.bw.Write(event[:n])

		default:
			// message_start, ping, error, message_stop, comments — pass through.
			w.bw.Write(event[:n])
		}
		w.buf.Next(n)
	}
	if err := w.bw.Flush(); err != nil {
		return 0, err
	}
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return len(data), nil
}

// shouldInject reports whether a message_delta event marks a natural turn end
// with at least one preceding content block to attach the footer after.
func (w *AnthropicRoutingFooterWriter) shouldInject(messageDelta []byte) bool {
	if w.maxIndex < 0 {
		return false
	}
	_, ok := naturalStopReasons[gjson.GetBytes(messageDelta, "delta.stop_reason").String()]
	return ok
}

// emitFooterBlock writes the footer as a standalone text content block at the
// given index (start + single text_delta + stop).
func (w *AnthropicRoutingFooterWriter) emitFooterBlock(index int) {
	w.bw.WriteString(`event: content_block_start
data: {"type":"content_block_start","index":`)
	sse.WriteJSONInt(w.bw, int64(index))
	w.bw.WriteString(`,"content_block":{"type":"text","text":""}}` + "\n\n")

	w.bw.WriteString(`event: content_block_delta
data: {"type":"content_block_delta","index":`)
	sse.WriteJSONInt(w.bw, int64(index))
	w.bw.WriteString(`,"delta":{"type":"text_delta","text":`)
	sse.WriteJSONString(w.bw, w.footer)
	w.bw.WriteString(`}}` + "\n\n")

	w.bw.WriteString(`event: content_block_stop
data: {"type":"content_block_stop","index":`)
	sse.WriteJSONInt(w.bw, int64(index))
	w.bw.WriteString(`}` + "\n\n")
}

var _ http.ResponseWriter = (*AnthropicRoutingFooterWriter)(nil)
var _ http.Flusher = (*AnthropicRoutingFooterWriter)(nil)
