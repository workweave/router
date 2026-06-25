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

// AnthropicRoutingFooterWriter wraps an http.ResponseWriter and appends the
// footer as a trailing text_delta INTO the final text content block of an
// Anthropic-format SSE stream — right before that block's content_block_stop —
// so the footer becomes part of the answer block itself rather than a separate
// block after it.
//
// Appending into the block (instead of emitting a new block at maxIndex+1)
// matters for clients that render only the LAST content block as the headline
// message: Conductor and other Claude Code wrappers surface content[last].text
// as the chat bubble and relegate earlier blocks to a trace view. A standalone
// footer block becomes content[last], so the user sees the rating prompt as the
// whole message and has to open the trace to read the real answer. Folding the
// footer into the answer block keeps content[last] == "<answer><footer>", which
// renders correctly both there and in the Claude Code TUI (which concatenates
// all text blocks). This mirrors how the OpenAI/Gemini footer writers coalesce
// the footer into the single answer stream.
//
// Non-streaming responses and an empty footer pass through untouched, and the
// footer is injected only when the turn completes naturally (see
// naturalStopReasons) and the final block is text, so tool-call turns stay clean.
type AnthropicRoutingFooterWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	footer string

	buf bytes.Buffer

	streaming      bool
	headersEmitted bool
	footerEmitted  bool

	// pendingStop holds the most recent content_block_stop event, deferred until
	// the next event reveals whether its block is the last one (and thus the
	// footer target). nil when no stop is currently held.
	pendingStop []byte
	// pendingIndex / pendingIsText describe the block whose stop is held: its
	// content_block index and whether it is a text block (the only kind we fold
	// a text footer into).
	pendingIndex  int
	pendingIsText bool
	// curType is the type ("text", "tool_use", "thinking", …) of the most
	// recently started content block, used to classify pendingStop on its close.
	curType string
}

// NewAnthropicRoutingFooterWriter wraps w so that footer is folded into the
// final text content block of a streamed Anthropic response. If footer is
// empty, all writes pass through unchanged.
func NewAnthropicRoutingFooterWriter(w http.ResponseWriter, footer string) *AnthropicRoutingFooterWriter {
	flusher, _ := w.(http.Flusher)
	return &AnthropicRoutingFooterWriter{
		inner:   w,
		flusher: flusher,
		bw:      bufio.NewWriterSize(w, 4096),
		footer:  footer,
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

// processUpstream parses the downstream Anthropic SSE, holds each
// content_block_stop until the following event reveals whether its block is the
// last one, and on a natural turn end folds the footer into that final text
// block as an extra text_delta emitted just before its content_block_stop.
// Everything is forwarded in order; the only mutation is the inserted text_delta.
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
		case "content_block_start":
			// A new block starting means any held stop closed a non-final block.
			w.flushPendingStop()
			w.curType = gjson.GetBytes(eventData, "content_block.type").String()
			w.bw.Write(event[:n])

		case "content_block_delta":
			w.flushPendingStop()
			w.bw.Write(event[:n])

		case "content_block_stop":
			// Defer the stop: if this turns out to be the final block we want to
			// inject the footer text_delta before it, not after.
			w.flushPendingStop()
			w.pendingStop = append([]byte(nil), event[:n]...)
			w.pendingIndex = int(gjson.GetBytes(eventData, "index").Int())
			w.pendingIsText = w.curType == "text"

		case "message_delta":
			if !w.footerEmitted && w.pendingStop != nil && w.pendingIsText && w.naturalStop(eventData) {
				w.emitFooterDelta(w.pendingIndex)
				w.footerEmitted = true
			}
			w.flushPendingStop()
			w.bw.Write(event[:n])

		case "ping":
			// Heartbeat pings carry no ordering significance, so forward them
			// without disturbing a held content_block_stop — a ping landing
			// between the final block's stop and message_delta must not force
			// the footer out of that block.
			w.bw.Write(event[:n])

		default:
			// message_start, error, message_stop, comments — flush any held stop
			// first so a non-natural turn end still emits a well-formed stream.
			w.flushPendingStop()
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

// flushPendingStop writes out a deferred content_block_stop, if any.
func (w *AnthropicRoutingFooterWriter) flushPendingStop() {
	if w.pendingStop == nil {
		return
	}
	w.bw.Write(w.pendingStop)
	w.pendingStop = nil
}

// naturalStop reports whether a message_delta marks a turn ending with a real,
// user-facing answer (see naturalStopReasons).
func (w *AnthropicRoutingFooterWriter) naturalStop(messageDelta []byte) bool {
	_, ok := naturalStopReasons[gjson.GetBytes(messageDelta, "delta.stop_reason").String()]
	return ok
}

// emitFooterDelta appends the footer to an existing text block as one more
// text_delta at the given index. It is emitted after the block's last upstream
// delta and before its (deferred) content_block_stop, so the footer renders as
// the tail of the answer block.
func (w *AnthropicRoutingFooterWriter) emitFooterDelta(index int) {
	w.bw.WriteString(`event: content_block_delta
data: {"type":"content_block_delta","index":`)
	sse.WriteJSONInt(w.bw, int64(index))
	w.bw.WriteString(`,"delta":{"type":"text_delta","text":`)
	sse.WriteJSONString(w.bw, w.footer)
	w.bw.WriteString(`}}` + "\n\n")
}

var _ http.ResponseWriter = (*AnthropicRoutingFooterWriter)(nil)
var _ http.Flusher = (*AnthropicRoutingFooterWriter)(nil)
