package translate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"workweave/router/internal/sse"

	"github.com/tidwall/gjson"
)

// ResponsesToChatCompletions converts an OpenAI Responses API request body into
// an equivalent Chat Completions request body so the existing chat-completions
// proxy path can dispatch it unchanged. Returns the rewritten body, whether the
// caller asked to stream, and the requested model (empty if absent).
//
// Only the subset of the Responses spec that Codex actually emits is handled:
// instructions, input items (message / function_call / function_call_output),
// tools (flat shape → nested function shape), tool_choice, reasoning.effort,
// max_output_tokens, temperature, top_p, parallel_tool_calls, metadata.
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
	if effort := root.Get("reasoning.effort").Str; effort != "" {
		out["reasoning_effort"] = effort
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

// responsesContentToChatContent flattens a content array. For assistant
// messages we may also extract tool-call shells if a client embeds them.
func responsesContentToChatContent(content gjson.Result, role string) (string, []map[string]any) {
	if content.Type == gjson.String {
		return content.Str, nil
	}
	if !content.IsArray() {
		return "", nil
	}
	var text strings.Builder
	var toolCalls []map[string]any
	for _, part := range content.Array() {
		switch part.Get("type").Str {
		case "input_text", "output_text", "text":
			text.WriteString(part.Get("text").Str)
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

	model      string
	responseID string
	createdAt  int64

	statusCode int
	streaming  bool
	buf        bytes.Buffer

	seq int64

	// Streaming state.
	headersEmitted bool
	textItem       *responsesTextItem
	toolItems      map[int]*responsesToolItem
	finishReason   string
	usage          *responsesUsage
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
		inner:      w,
		flusher:    flusher,
		bw:         bufio.NewWriterSize(w, 8192),
		model:      model,
		responseID: newResponsesID("resp"),
		createdAt:  time.Now().Unix(),
		toolItems:  map[int]*responsesToolItem{},
	}
}

func (t *ResponsesWriter) Header() http.Header { return t.inner.Header() }

func (t *ResponsesWriter) WriteHeader(code int) {
	t.statusCode = code
	ct := t.inner.Header().Get("Content-Type")
	t.streaming = strings.Contains(ct, "text/event-stream") && code < 400

	// The proxy stamps the routing decision on the response headers before the
	// downstream writer is given any bytes. Prefer the routed model name so
	// Codex's TUI (and any other client that reads `response.model`) displays
	// what the router actually picked instead of the requested alias.
	if routed := t.inner.Header().Get("x-router-model"); routed != "" {
		t.model = routed
	}

	t.inner.Header().Del("Content-Length")
	t.inner.Header().Del("Content-Encoding")

	if t.streaming {
		t.inner.Header().Set("Content-Type", "text/event-stream")
		t.inner.WriteHeader(code)
	}
}

func (t *ResponsesWriter) Write(data []byte) (int, error) {
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
	if t.streaming {
		// In the rare case the upstream produced no chunks at all, still
		// emit a completed envelope so the client sees a clean termination.
		if !t.headersEmitted {
			if err := t.emitCreated(); err != nil {
				return err
			}
			t.headersEmitted = true
		}
		t.closeOpenItems()
		if err := t.emitCompleted(); err != nil {
			return err
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
			itemID:      newResponsesID("msg"),
			outputIndex: t.nextOutputIndex(),
		}
		if err := t.emitMessageItemAdded(t.textItem); err != nil {
			return err
		}
		if err := t.emitContentPartAdded(t.textItem); err != nil {
			return err
		}
		t.textItem.openedPart = true
	}
	t.textItem.text.WriteString(s)
	return t.emitTextDelta(t.textItem, s)
}

func (t *ResponsesWriter) appendToolCall(idx int, tc gjson.Result) error {
	item, ok := t.toolItems[idx]
	if !ok {
		item = &responsesToolItem{
			itemID:      newResponsesID("fc"),
			outputIndex: t.nextOutputIndex(),
		}
		t.toolItems[idx] = item
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

func (t *ResponsesWriter) assembleOutput() []any {
	out := make([]any, 0, 1+len(t.toolItems))
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
