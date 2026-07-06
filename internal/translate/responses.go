package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// ResponsesToChatCompletions converts an OpenAI Responses API request into a
// Chat Completions request so the existing proxy path can dispatch it
// unchanged. Returns the rewritten body, whether streaming was requested, and
// the requested model (empty if absent). Handles only the subset of the
// Responses spec Codex actually emits: instructions, input items (message /
// function_call / function_call_output), tools, tool_choice,
// max_output_tokens, temperature, top_p, parallel_tool_calls, metadata.
//
// Codex's `reasoning` field is dropped intentionally: this runs before
// routing, so the served provider (and its native thinking knob) is unknown.
// Forwarding it as `reasoning_effort` broke every non-Gemini model — Codex
// sends invalid values (e.g. "none"), and gpt-5.x rejects `reasoning_effort`
// alongside tools, both causing an upstream 400 mid-stream. Per-provider
// reasoning is still driven downstream from the request's own signals.
func ResponsesToChatCompletions(body []byte) ([]byte, bool, string, error) {
	if err := validateJSONObject(body); err != nil {
		return nil, false, "", err
	}

	root := gjson.ParseBytes(body)
	out := map[string]any{}

	model := root.Get("model").Str
	if model != "" {
		out["model"] = model
	}

	stream := root.Get("stream").Bool()
	out["stream"] = stream
	if stream {
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	messages := make([]map[string]any, 0, 8)
	if instructions := root.Get("instructions").Str; instructions != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": instructions,
		})
	}

	input := root.Get("input")
	switch {
	case input.IsArray():
		for _, item := range input.Array() {
			msgs, err := responsesInputItemToMessages(item)
			if err != nil {
				return nil, false, "", err
			}
			messages = append(messages, msgs...)
		}
	case input.Type == gjson.String:
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": input.Str,
		})
	}
	out["messages"] = messages

	if tools := root.Get("tools"); tools.IsArray() {
		converted := make([]map[string]any, 0, len(tools.Array()))
		for _, t := range tools.Array() {
			// Responses tools are flat: {type, name, description, parameters, strict}.
			// Chat Completions nest under {type:"function", function:{...}}.
			if t.Get("type").Str != "function" {
				continue
			}
			fn := map[string]any{}
			if name := t.Get("name").Str; name != "" {
				fn["name"] = name
			} else if name := t.Get("function.name").Str; name != "" {
				fn["name"] = name
			}
			if desc := t.Get("description").Str; desc != "" {
				fn["description"] = desc
			} else if desc := t.Get("function.description").Str; desc != "" {
				fn["description"] = desc
			}
			if params := t.Get("parameters"); params.Exists() {
				fn["parameters"] = json.RawMessage(params.Raw)
			} else if params := t.Get("function.parameters"); params.Exists() {
				fn["parameters"] = json.RawMessage(params.Raw)
			}
			if strict := t.Get("strict"); strict.Exists() {
				fn["strict"] = strict.Bool()
			}
			converted = append(converted, map[string]any{
				"type":     "function",
				"function": fn,
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}

	if tc := root.Get("tool_choice"); tc.Exists() {
		out["tool_choice"] = json.RawMessage(tc.Raw)
	}
	if pt := root.Get("parallel_tool_calls"); pt.Exists() {
		out["parallel_tool_calls"] = pt.Bool()
	}
	if temp := root.Get("temperature"); temp.Exists() {
		out["temperature"] = temp.Num
	}
	if topP := root.Get("top_p"); topP.Exists() {
		out["top_p"] = topP.Num
	}
	if max := root.Get("max_output_tokens"); max.Exists() {
		out["max_completion_tokens"] = max.Int()
	}
	if md := root.Get("metadata"); md.IsObject() {
		out["metadata"] = json.RawMessage(md.Raw)
	}

	bodyOut, err := json.Marshal(out)
	if err != nil {
		return nil, false, "", fmt.Errorf("marshal chat completions: %w", err)
	}
	return bodyOut, stream, model, nil
}

// responsesInputItemToMessages flattens a single Responses input item into one
// or more Chat Completions messages. Returns ([], nil) for item types we don't
// recognize so unknown future shapes don't fail the whole request.
func responsesInputItemToMessages(item gjson.Result) ([]map[string]any, error) {
	itemType := item.Get("type").Str
	// Some Responses clients omit "type" and send a bare chat-style {role, content}.
	if itemType == "" {
		if role := item.Get("role").Str; role != "" {
			itemType = "message"
		}
	}

	switch itemType {
	case "message":
		role := item.Get("role").Str
		if role == "" {
			role = "user"
		}
		text, toolCalls := responsesContentToChatContent(item.Get("content"), role)
		msg := map[string]any{"role": role}
		if text != "" || len(toolCalls) == 0 {
			msg["content"] = text
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []map[string]any{msg}, nil

	case "function_call":
		call := map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":      item.Get("name").Str,
				"arguments": item.Get("arguments").Str,
			},
		}
		if id := item.Get("call_id").Str; id != "" {
			call["id"] = id
		} else if id := item.Get("id").Str; id != "" {
			call["id"] = id
		}
		return []map[string]any{{
			"role":       "assistant",
			"content":    "",
			"tool_calls": []map[string]any{call},
		}}, nil

	case "function_call_output":
		out := map[string]any{
			"role":    "tool",
			"content": item.Get("output").Str,
		}
		if id := item.Get("call_id").Str; id != "" {
			out["tool_call_id"] = id
		}
		return []map[string]any{out}, nil

	case "reasoning":
		// Drop reasoning items on the inbound path; assistant summaries aren't
		// re-fed to chat-completions providers in a portable way.
		return nil, nil
	}
	return nil, nil
}

// responsesBadgePattern matches the routing badge ResponsesWriter prepends to
// the first assistant text delta. Stripped on ingress so it doesn't accumulate
// in history (defeats prompt-cache reuse) or leak router-injected content
// upstream.
var responsesBadgePattern = regexp.MustCompile(`(?im)\A\*\*WEAVE ROUTER\*\* — [^\n]*\n\n`)

// responsesContentToChatContent flattens a content array. For assistant
// messages we may also extract tool-call shells if a client embeds them.
func responsesContentToChatContent(content gjson.Result, role string) (string, []map[string]any) {
	if content.Type == gjson.String {
		s := content.Str
		if role == "assistant" {
			s = responsesBadgePattern.ReplaceAllString(s, "")
		}
		return s, nil
	}
	if !content.IsArray() {
		return "", nil
	}
	var text strings.Builder
	var toolCalls []map[string]any
	firstAssistantTextStripped := false
	for _, part := range content.Array() {
		switch part.Get("type").Str {
		case "input_text", "output_text", "text":
			s := part.Get("text").Str
			// Strip only the assistant's first output_text part, so user/system
			// text is never touched even if it happens to start with marker bytes.
			if role == "assistant" && !firstAssistantTextStripped {
				s = responsesBadgePattern.ReplaceAllString(s, "")
				firstAssistantTextStripped = true
			}
			text.WriteString(s)
		case "refusal":
			text.WriteString(part.Get("refusal").Str)
		case "tool_use":
			if role == "assistant" {
				toolCalls = append(toolCalls, map[string]any{
					"id":   part.Get("id").Str,
					"type": "function",
					"function": map[string]any{
						"name":      part.Get("name").Str,
						"arguments": part.Get("arguments").Str,
					},
				})
			}
		}
	}
	return text.String(), toolCalls
}

// ResponsesWriter wraps an http.ResponseWriter and translates a downstream
// Chat Completions response (streaming SSE or buffered JSON) into the
// Responses API shape on the fly.
type ResponsesWriter struct {
	inner   http.ResponseWriter
	flusher http.Flusher
	bw      *bufio.Writer

	model          string // routed model, set from x-router-model when known
	requestedModel string // originally requested model (from the client's request)
	responseID     string
	createdAt      int64

	statusCode      int
	streaming       bool
	httpHeadersSent bool
	passthrough     bool
	buf             bytes.Buffer

	seq int64

	// Streaming state.
	headersEmitted   bool
	completedEmitted bool
	badgePrepended   bool
	textItem         *responsesTextItem
	toolItems        map[int]*responsesToolItem
	finishReason     string
	usage            *responsesUsage
}

type responsesTextItem struct {
	itemID      string
	outputIndex int
	openedPart  bool
	text        strings.Builder
	closed      bool
}

type responsesToolItem struct {
	itemID      string
	callID      string
	name        string
	outputIndex int
	arguments   strings.Builder
	closed      bool
}

type responsesUsage struct {
	prompt     int64
	completion int64
	total      int64
}

var responsesIDCounter atomic.Uint64

// newResponsesID returns a short opaque ID. Codex doesn't read these for
// correctness; they exist to give the SSE stream stable handles per item.
func newResponsesID(prefix string) string {
	return prefix + "_" + strconv.FormatUint(responsesIDCounter.Add(1), 36) + strconv.FormatInt(time.Now().UnixNano()&0xffffff, 36)
}

// NewResponsesWriter wraps w so chat-completions output is re-emitted as
// Responses API events.
func NewResponsesWriter(w http.ResponseWriter, model string) *ResponsesWriter {
	flusher, _ := w.(http.Flusher)
	return &ResponsesWriter{
		inner:          w,
		flusher:        flusher,
		bw:             bufio.NewWriterSize(w, 8192),
		model:          model,
		requestedModel: model,
		responseID:     newResponsesID("resp"),
		createdAt:      time.Now().Unix(),
		toolItems:      map[int]*responsesToolItem{},
	}
}

// WrapInner splices fn between this writer and the client writer, rebinding
// inner and bw so every byte (prelude, SSE events, final envelope) flows
// through fn — used for content-capture telemetry. Call before any writes.
func (t *ResponsesWriter) WrapInner(fn func(http.ResponseWriter) http.ResponseWriter) {
	wrapped := fn(t.inner)
	t.inner = wrapped
	t.flusher, _ = wrapped.(http.Flusher)
	t.bw = bufio.NewWriterSize(wrapped, 8192)
}

func (t *ResponsesWriter) Header() http.Header { return t.inner.Header() }

// SetPassthrough switches to verbatim mode: bytes forwarded unchanged, no
// chat->Responses translation, no response.created prelude. Use when upstream
// already speaks Responses natively (Codex backend) — re-translating would
// corrupt the stream. Must be called before the first write (right after
// routing, before Prelude).
func (t *ResponsesWriter) SetPassthrough() { t.passthrough = true }

func (t *ResponsesWriter) WriteHeader(code int) {
	if t.passthrough {
		if t.httpHeadersSent {
			return
		}
		t.statusCode = code
		// Codex backend already sets text/event-stream; only drop length/encoding.
		t.inner.Header().Del("Content-Length")
		t.inner.Header().Del("Content-Encoding")
		t.inner.WriteHeader(code)
		t.httpHeadersSent = true
		return
	}
	// Prelude fires before routing completes (x-router-model unset yet), so
	// this later call is our only chance to learn the routed name.
	if routed := t.inner.Header().Get("x-router-model"); routed != "" {
		t.model = routed
	}
	if t.httpHeadersSent {
		return
	}
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
		t.httpHeadersSent = true
	}
}

func (t *ResponsesWriter) Write(data []byte) (int, error) {
	if t.passthrough {
		// Forward verbatim. The upstream (Codex backend) emits Responses SSE
		// natively, so there is nothing to translate.
		if !t.httpHeadersSent {
			t.inner.WriteHeader(http.StatusOK)
			t.httpHeadersSent = true
		}
		written, err := t.bw.Write(data)
		if err == nil {
			err = t.bw.Flush()
			if t.flusher != nil {
				t.flusher.Flush()
			}
		}
		return written, err
	}
	n := len(data)
	t.buf.Write(data)
	if !t.streaming {
		return n, nil
	}
	if !t.headersEmitted {
		if err := t.emitCreated(); err != nil {
			return n, err
		}
		t.headersEmitted = true
	}
	return n, t.processSSEBuffer()
}

// Prelude commits headers and emits response.created immediately so Codex
// stops waiting on upstream prefill. Call right after routing when streaming
// was requested; the headersEmitted guard makes it safe to call once, with
// upstream Write emitting created later if this hasn't run yet.
func (t *ResponsesWriter) Prelude(streaming bool) error {
	// Upstream emits its own response.created in passthrough mode.
	if t.passthrough {
		return nil
	}
	if !streaming || t.headersEmitted {
		return nil
	}
	t.inner.Header().Set("Content-Type", "text/event-stream")
	t.streaming = true
	t.statusCode = http.StatusOK
	if !t.httpHeadersSent {
		t.inner.WriteHeader(http.StatusOK)
		t.httpHeadersSent = true
	}
	t.headersEmitted = true
	return t.emitCreated()
}

func (t *ResponsesWriter) Flush() {
	if !t.streaming {
		return
	}
	_ = t.bw.Flush()
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

// Finalize handles non-streaming bodies and end-of-stream completion events.
func (t *ResponsesWriter) Finalize() error {
	if t.passthrough {
		// Nothing to synthesize; bodies were already forwarded as-is in Write.
		return t.bw.Flush()
	}
	if t.streaming {
		// Upstream may have produced zero chunks; still emit a clean completed envelope.
		if !t.headersEmitted {
			if err := t.emitCreated(); err != nil {
				return err
			}
			t.headersEmitted = true
		}
		t.closeOpenItems()
		if !t.completedEmitted {
			if err := t.emitCompleted(); err != nil {
				return err
			}
			t.completedEmitted = true
		}
		return t.bw.Flush()
	}

	body := t.buf.Bytes()
	if t.statusCode >= 400 {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(t.statusCode)
		_, err := t.inner.Write(body)
		return err
	}

	translated, err := chatCompletionToResponse(body, t.responseID, t.model, t.createdAt)
	if err != nil {
		t.inner.Header().Set("Content-Type", "application/json")
		t.inner.WriteHeader(http.StatusBadGateway)
		_, _ = t.inner.Write([]byte(`{"error":{"message":"translation failed","type":"api_error"}}`))
		return err
	}
	t.inner.Header().Set("Content-Type", "application/json")
	t.inner.WriteHeader(t.statusCode)
	_, err = t.inner.Write(translated)
	return err
}

// FinalizeError emits a response.failed terminal event when upstream fails
// mid-stream (after response.created), so Codex sees a clean failure instead
// of a truncated stream. No-op if nothing streamed yet (caller writes a JSON
// error instead), in passthrough mode, or after a terminal event already fired.
func (t *ResponsesWriter) FinalizeError(_ error) error {
	if t.passthrough || !t.streaming || !t.headersEmitted || t.completedEmitted {
		return nil
	}
	t.closeOpenItems()
	if err := t.emitFailed(); err != nil {
		return err
	}
	t.completedEmitted = true
	return t.bw.Flush()
}

// processSSEBuffer drains complete chat.completion.chunk events.
func (t *ResponsesWriter) processSSEBuffer() error {
	for {
		event, n := sse.SplitNext(t.buf.Bytes())
		if n == 0 {
			return nil
		}
		err := t.translateChunk(event)
		t.buf.Next(n)
		if err != nil {
			return err
		}
	}
}

func (t *ResponsesWriter) translateChunk(raw []byte) error {
	_, data := sse.ParseEvent(raw)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		t.closeOpenItems()
		if t.completedEmitted {
			return nil
		}
		t.completedEmitted = true
		return t.emitCompleted()
	}
	if !gjson.ValidBytes(data) {
		return nil
	}

	root := gjson.ParseBytes(data)
	if m := root.Get("model").Str; m != "" && t.model == "" {
		t.model = strings.Clone(m)
	}
	if usage := root.Get("usage"); usage.Exists() {
		t.usage = &responsesUsage{
			prompt:     usage.Get("prompt_tokens").Int(),
			completion: usage.Get("completion_tokens").Int(),
			total:      usage.Get("total_tokens").Int(),
		}
	}

	choice := root.Get("choices.0")
	if !choice.Exists() {
		return nil
	}
	delta := choice.Get("delta")

	if content := delta.Get("content"); content.Type == gjson.String && content.Str != "" {
		if err := t.appendText(content.Str); err != nil {
			return err
		}
	}

	if tcs := delta.Get("tool_calls"); tcs.IsArray() {
		for _, tc := range tcs.Array() {
			idx := int(tc.Get("index").Int())
			if err := t.appendToolCall(idx, tc); err != nil {
				return err
			}
		}
	}

	if fr := choice.Get("finish_reason"); fr.Type == gjson.String && fr.Str != "" {
		t.finishReason = fr.Str
		t.closeOpenItems()
	}
	return nil
}

func (t *ResponsesWriter) appendText(s string) error {
	if t.textItem == nil {
		t.textItem = &responsesTextItem{
			itemID: newResponsesID("msg"),
		}
		// Must assign after t.textItem is reachable, else nextOutputIndex
		// undercounts it (Go evaluates struct-literal RHS before assignment).
		t.textItem.outputIndex = t.nextOutputIndex()
		if err := t.emitMessageItemAdded(t.textItem); err != nil {
			return err
		}
		if err := t.emitContentPartAdded(t.textItem); err != nil {
			return err
		}
		t.textItem.openedPart = true
	}

	// Prepend the badge to the first text delta: Codex desktop drops
	// reasoning-summary items from custom providers, so text is the only
	// surface guaranteed to render.
	if !t.badgePrepended {
		t.badgePrepended = true
		if line := t.computeBadgeText(); line != "" {
			t.textItem.text.WriteString(line)
			if err := t.emitTextDelta(t.textItem, line); err != nil {
				return err
			}
		}
	}

	t.textItem.text.WriteString(s)
	return t.emitTextDelta(t.textItem, s)
}

func (t *ResponsesWriter) appendToolCall(idx int, tc gjson.Result) error {
	item, ok := t.toolItems[idx]
	if !ok {
		item = &responsesToolItem{
			itemID: newResponsesID("fc"),
		}
		t.toolItems[idx] = item
		item.outputIndex = t.nextOutputIndex()
		if id := tc.Get("id").Str; id != "" {
			item.callID = strings.Clone(id)
		} else {
			item.callID = newResponsesID("call")
		}
		if name := tc.Get("function.name").Str; name != "" {
			item.name = strings.Clone(name)
		}
		if err := t.emitFunctionCallItemAdded(item); err != nil {
			return err
		}
	}
	// Later chunks may carry name/id only on first delta; pick up any later
	// arrivals defensively.
	if item.name == "" {
		if name := tc.Get("function.name").Str; name != "" {
			item.name = strings.Clone(name)
		}
	}
	if args := tc.Get("function.arguments").Str; args != "" {
		item.arguments.WriteString(args)
		return t.emitFunctionArgsDelta(item, args)
	}
	return nil
}

func (t *ResponsesWriter) nextOutputIndex() int {
	count := 0
	if t.textItem != nil {
		count++
	}
	count += len(t.toolItems)
	return count - 1
}

// computeBadgeText builds the badge prepended to the assistant's first text
// delta, e.g. "**Weave Router** — <routed> ← <requested>" (arrow only when
// swapped). Returns "" if no routed model is known yet.
func (t *ResponsesWriter) computeBadgeText() string {
	if t.model == "" {
		return ""
	}
	badge := "**Weave Router** — " + t.model
	if t.requestedModel != "" && t.requestedModel != t.model {
		badge += " ← " + t.requestedModel
	}
	return badge + "\n\n"
}

func (t *ResponsesWriter) closeOpenItems() {
	if t.textItem != nil && !t.textItem.closed {
		_ = t.emitTextDone(t.textItem)
		_ = t.emitContentPartDone(t.textItem)
		_ = t.emitMessageItemDone(t.textItem)
		t.textItem.closed = true
	}
	for _, item := range t.toolItems {
		if item.closed {
			continue
		}
		_ = t.emitFunctionArgsDone(item)
		_ = t.emitFunctionCallItemDone(item)
		item.closed = true
	}
}

// ---------- event emitters ----------

func (t *ResponsesWriter) nextSeq() int64 {
	s := t.seq
	t.seq++
	return s
}

func (t *ResponsesWriter) writeEvent(eventType string, payload map[string]any) error {
	payload["type"] = eventType
	payload["sequence_number"] = t.nextSeq()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := t.bw.WriteString("event: "); err != nil {
		return err
	}
	if _, err := t.bw.WriteString(eventType); err != nil {
		return err
	}
	if _, err := t.bw.WriteString("\ndata: "); err != nil {
		return err
	}
	if _, err := t.bw.Write(body); err != nil {
		return err
	}
	if _, err := t.bw.WriteString("\n\n"); err != nil {
		return err
	}
	if err := t.bw.Flush(); err != nil {
		return err
	}
	if t.flusher != nil {
		t.flusher.Flush()
	}
	return nil
}

func (t *ResponsesWriter) responseEnvelope(status string) map[string]any {
	env := map[string]any{
		"id":         t.responseID,
		"object":     "response",
		"created_at": t.createdAt,
		"status":     status,
		"model":      t.model,
	}
	return env
}

func (t *ResponsesWriter) emitCreated() error {
	return t.writeEvent("response.created", map[string]any{
		"response": t.responseEnvelope("in_progress"),
	})
}

func (t *ResponsesWriter) emitMessageItemAdded(item *responsesTextItem) error {
	return t.writeEvent("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item": map[string]any{
			"id":      item.itemID,
			"type":    "message",
			"status":  "in_progress",
			"role":    "assistant",
			"content": []any{},
		},
	})
}

func (t *ResponsesWriter) emitContentPartAdded(item *responsesTextItem) error {
	return t.writeEvent("response.content_part.added", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
		},
	})
}

func (t *ResponsesWriter) emitTextDelta(item *responsesTextItem, delta string) error {
	return t.writeEvent("response.output_text.delta", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"delta":         delta,
	})
}

func (t *ResponsesWriter) emitTextDone(item *responsesTextItem) error {
	return t.writeEvent("response.output_text.done", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"text":          item.text.String(),
	})
}

func (t *ResponsesWriter) emitContentPartDone(item *responsesTextItem) error {
	return t.writeEvent("response.content_part.done", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        item.text.String(),
			"annotations": []any{},
		},
	})
}

func (t *ResponsesWriter) emitMessageItemDone(item *responsesTextItem) error {
	return t.writeEvent("response.output_item.done", map[string]any{
		"output_index": item.outputIndex,
		"item": map[string]any{
			"id":     item.itemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        item.text.String(),
				"annotations": []any{},
			}},
		},
	})
}

func (t *ResponsesWriter) emitFunctionCallItemAdded(item *responsesToolItem) error {
	return t.writeEvent("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item": map[string]any{
			"id":        item.itemID,
			"type":      "function_call",
			"status":    "in_progress",
			"call_id":   item.callID,
			"name":      item.name,
			"arguments": "",
		},
	})
}

func (t *ResponsesWriter) emitFunctionArgsDelta(item *responsesToolItem, delta string) error {
	return t.writeEvent("response.function_call_arguments.delta", map[string]any{
		"item_id":      item.itemID,
		"output_index": item.outputIndex,
		"delta":        delta,
	})
}

func (t *ResponsesWriter) emitFunctionArgsDone(item *responsesToolItem) error {
	return t.writeEvent("response.function_call_arguments.done", map[string]any{
		"item_id":      item.itemID,
		"output_index": item.outputIndex,
		"arguments":    item.arguments.String(),
	})
}

func (t *ResponsesWriter) emitFunctionCallItemDone(item *responsesToolItem) error {
	return t.writeEvent("response.output_item.done", map[string]any{
		"output_index": item.outputIndex,
		"item": map[string]any{
			"id":        item.itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   item.callID,
			"name":      item.name,
			"arguments": item.arguments.String(),
		},
	})
}

func (t *ResponsesWriter) emitCompleted() error {
	env := t.responseEnvelope("completed")
	env["output"] = t.assembleOutput()
	if t.usage != nil {
		env["usage"] = map[string]any{
			"input_tokens":  t.usage.prompt,
			"output_tokens": t.usage.completion,
			"total_tokens":  t.usage.total,
		}
	}
	return t.writeEvent("response.completed", map[string]any{
		"response": env,
	})
}

// emitFailed writes response.failed with whatever output was assembled so far
// plus a generic error (no upstream internals leak). Usage omitted: no
// trustworthy accounting for a failed turn.
func (t *ResponsesWriter) emitFailed() error {
	env := t.responseEnvelope("failed")
	env["output"] = t.assembleOutput()
	env["error"] = map[string]any{
		"code":    "upstream_error",
		"message": "Upstream call failed.",
	}
	return t.writeEvent("response.failed", map[string]any{
		"response": env,
	})
}

func (t *ResponsesWriter) assembleOutput() []any {
	out := make([]any, 0, len(t.toolItems))
	if t.textItem != nil {
		out = append(out, map[string]any{
			"id":     t.textItem.itemID,
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        t.textItem.text.String(),
				"annotations": []any{},
			}},
		})
	}
	// Tool items in original index order.
	for i := 0; i < len(t.toolItems); i++ {
		item, ok := t.toolItems[i]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"id":        item.itemID,
			"type":      "function_call",
			"status":    "completed",
			"call_id":   item.callID,
			"name":      item.name,
			"arguments": item.arguments.String(),
		})
	}
	return out
}

// chatCompletionToResponse converts a buffered chat-completions JSON body into
// a Responses-shaped JSON body. Only used when the client requested
// stream:false; Codex always streams, but other clients may not.
func chatCompletionToResponse(body []byte, responseID, model string, createdAt int64) ([]byte, error) {
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("invalid JSON")
	}
	root := gjson.ParseBytes(body)
	if model == "" {
		model = root.Get("model").Str
	}

	out := map[string]any{
		"id":         responseID,
		"object":     "response",
		"created_at": createdAt,
		"status":     "completed",
		"model":      model,
	}

	choice := root.Get("choices.0.message")
	output := make([]any, 0, 2)
	if content := choice.Get("content"); content.Type == gjson.String && content.Str != "" {
		output = append(output, map[string]any{
			"id":     newResponsesID("msg"),
			"type":   "message",
			"status": "completed",
			"role":   "assistant",
			"content": []any{map[string]any{
				"type":        "output_text",
				"text":        content.Str,
				"annotations": []any{},
			}},
		})
	}
	if tcs := choice.Get("tool_calls"); tcs.IsArray() {
		for _, tc := range tcs.Array() {
			output = append(output, map[string]any{
				"id":        newResponsesID("fc"),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   tc.Get("id").Str,
				"name":      tc.Get("function.name").Str,
				"arguments": tc.Get("function.arguments").Str,
			})
		}
	}
	out["output"] = output

	if usage := root.Get("usage"); usage.Exists() {
		out["usage"] = map[string]any{
			"input_tokens":  usage.Get("prompt_tokens").Int(),
			"output_tokens": usage.Get("completion_tokens").Int(),
			"total_tokens":  usage.Get("total_tokens").Int(),
		}
	}

	return json.Marshal(out)
}
