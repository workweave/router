package translate

import (
	"net/http"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// OpenAIRoutingMarkerWriter wraps an http.ResponseWriter and injects a
// routing-marker chunk at the start of an OpenAI-format SSE stream.
// Non-streaming responses pass through unmodified.
type OpenAIRoutingMarkerWriter struct {
	sse.ChunkedWriter

	marker string
	model  string

	markerEmitted bool

	// onOutputProgress, when set via ArmOutputProgress, is invoked whenever a
	// written upstream chunk carries an output-bearing delta (content,
	// reasoning/reasoning_content, tool_calls, or a terminal finish_reason) and
	// never on keepalive comments or empty/role-only frames. This is the
	// OpenAI→OpenAI passthrough path's only place to tell output from keepalive,
	// since no translator parses the stream here. It feeds the openaicompat
	// client's output-progress watchdog (see httputil.DefaultOutputStallTimeout).
	// nil disables it.
	onOutputProgress func()
	// outputLeftover holds the unconsumed tail of the most recent Write so output
	// detection splits on SSE event boundaries that span Write calls. Complete
	// events are scanned and discarded immediately.
	outputLeftover []byte
}

// NewOpenAIRoutingMarkerWriter creates a writer that emits marker as the first
// chat.completion.chunk before any upstream data. If marker is empty, all
// writes pass through unchanged.
func NewOpenAIRoutingMarkerWriter(w http.ResponseWriter, model, marker string) *OpenAIRoutingMarkerWriter {
	return &OpenAIRoutingMarkerWriter{
		ChunkedWriter: sse.NewChunkedWriter(w, 4096),
		marker:        marker,
		model:         model,
	}
}

func (w *OpenAIRoutingMarkerWriter) Write(data []byte) (int, error) {
	if w.Streaming && !w.markerEmitted {
		w.markerEmitted = true
		if w.marker != "" {
			if err := w.emitMarkerChunk(); err != nil {
				return 0, err
			}
		}
	}
	if w.Streaming && w.onOutputProgress != nil {
		w.scanOutputProgress(data)
	}
	return w.Inner.Write(data)
}

// ArmOutputProgress installs the output-progress watchdog mark. The writer
// invokes mark whenever a written upstream chunk carries an output-bearing
// delta — assistant content, streamed reasoning, tool-call fragments, or a
// terminal finish_reason — and never on keepalive comments or empty/role-only
// frames. It returns false (and installs nothing) when the response is not
// streaming: a non-streaming passthrough has nothing to mark mid-stream. Call
// after WriteHeader / Prelude, which set the streaming flag.
func (w *OpenAIRoutingMarkerWriter) ArmOutputProgress(mark func()) (armed bool) {
	if !w.Streaming {
		return false
	}
	w.onOutputProgress = mark
	return true
}

// scanOutputProgress splits buffered passthrough bytes on SSE event boundaries
// and marks output progress for each complete event that carries an
// output-bearing delta. Unlike a translator it does not re-encode; it only
// classifies, so the raw OpenAI Chat Completions chunk is probed directly.
func (w *OpenAIRoutingMarkerWriter) scanOutputProgress(data []byte) {
	w.outputLeftover = append(w.outputLeftover, data...)
	buf := w.outputLeftover
	for {
		event, n := sse.SplitNext(buf)
		if n == 0 {
			break
		}
		_, payload := sse.ParseEvent(event)
		buf = buf[n:]
		if len(payload) == 0 {
			continue
		}
		if chunkCarriesOutput(payload) {
			w.onOutputProgress()
		}
	}
	rest := copy(w.outputLeftover, buf)
	w.outputLeftover = w.outputLeftover[:rest]
}

// chunkCarriesOutput reports whether a raw OpenAI Chat Completions SSE chunk
// payload carries model output: a non-empty content / reasoning delta, a
// tool_calls array, or a terminal finish_reason. Role-only opening deltas,
// null-valued deltas (the GLM-5.1 keepalive shape), usage-only chunks, and the
// [DONE] sentinel return false — matching the AnthropicSSETranslator's
// classification so the two openaicompat paths trip the watchdog identically.
func chunkCarriesOutput(payload []byte) bool {
	choice := gjson.GetBytes(payload, "choices.0")
	if !choice.Exists() {
		return false
	}
	if fr := choice.Get("finish_reason"); fr.Type == gjson.String && fr.Str != "" {
		return true
	}
	delta := choice.Get("delta")
	if !delta.Exists() {
		return false
	}
	if delta.Get("content").Str != "" {
		return true
	}
	if delta.Get("reasoning_content").Str != "" || delta.Get("reasoning").Str != "" {
		return true
	}
	if delta.Get("tool_calls.#").Int() > 0 {
		return true
	}
	return false
}

// Prelude commits headers and emits the routing marker immediately, before the
// upstream provider has returned a single byte. Call this right after the
// routing decision is made when the client requested streaming (streaming=true)
// so first-byte latency drops to ~routing time rather than upstream prefill +
// first decode. Safe to call once; subsequent Write/WriteHeader calls are
// idempotent. No-op when streaming is false.
func (w *OpenAIRoutingMarkerWriter) Prelude(streaming bool) error {
	if !streaming || w.markerEmitted {
		return nil
	}
	w.Inner.Header().Set("Content-Type", "text/event-stream")
	w.Streaming = true
	if !w.HeadersEmitted {
		w.HeadersEmitted = true
		w.Inner.WriteHeader(http.StatusOK)
	}
	w.markerEmitted = true
	if w.marker == "" {
		// Still flush a comment so TCP gets a packet — TTFB is what we're optimizing for.
		w.BW.WriteString(": routing complete\n\n")
		return w.FlushEvent()
	}
	return w.emitMarkerChunk()
}

func (w *OpenAIRoutingMarkerWriter) emitMarkerChunk() error {
	w.BW.WriteString(`data: {"id":`)
	sse.WriteJSONString(w.BW, generateChatCmplID())
	w.BW.WriteString(`,"object":"chat.completion.chunk","created":`)
	sse.WriteJSONInt(w.BW, time.Now().Unix())
	w.BW.WriteString(`,"model":`)
	sse.WriteJSONString(w.BW, w.model)
	w.BW.WriteString(`,"choices":[{"index":0,"delta":{"role":"assistant","content":`)
	sse.WriteJSONString(w.BW, w.marker)
	w.BW.WriteString(`},"finish_reason":null}]}`)
	w.BW.WriteString("\n\n")
	return w.FlushEvent()
}

var _ http.ResponseWriter = (*OpenAIRoutingMarkerWriter)(nil)
var _ http.Flusher = (*OpenAIRoutingMarkerWriter)(nil)
var _ providers.OutputProgressArmer = (*OpenAIRoutingMarkerWriter)(nil)
