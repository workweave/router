package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"

	"github.com/tidwall/gjson"
)

// PrepareAnthropic builds an Anthropic Messages request body.
func (e *RequestEnvelope) PrepareAnthropic(in http.Header, opts EmitOptions) (providers.PreparedRequest, error) {
	var body []byte
	var err error
	switch e.format {
	case FormatOpenAI:
		var out map[string]any
		out, err = e.buildAnthropicFromOpenAI(opts)
		if err != nil {
			return providers.PreparedRequest{}, fmt.Errorf("build anthropic from openai: %w", err)
		}
		body, err = json.Marshal(out)
	case FormatAnthropic:
		body, err = e.buildAnthropicFromAnthropic(opts)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unsupported source format for Anthropic emit: %d", e.format)
	}
	if err != nil {
		return providers.PreparedRequest{}, fmt.Errorf("marshal anthropic body: %w", err)
	}
	return providers.PreparedRequest{Body: body, Headers: deriveAnthropicHeaders(in, opts)}, nil
}

func deriveAnthropicHeaders(in http.Header, opts EmitOptions) http.Header {
	h := make(http.Header)
	if v := in.Get("anthropic-version"); v != "" {
		h.Set("anthropic-version", v)
	} else {
		h.Set("anthropic-version", "2023-06-01")
	}
	if v := filterBetaHeader(in.Get("anthropic-beta"), opts.TargetModel); v != "" {
		h.Set("anthropic-beta", v)
	}
	return h
}

func filterBetaHeader(beta, targetModel string) string {
	if beta == "" {
		return ""
	}
	spec := router.Lookup(targetModel)
	return joinKept(beta, func(token string) bool {
		return betaCompatible(token, spec)
	})
}

func betaCompatible(token string, spec router.ModelSpec) bool {
	if strings.Contains(token, "interleaved-thinking") ||
		strings.Contains(token, "adaptive-thinking") {
		return spec.Supports(router.CapAdaptiveThinking)
	}
	if strings.Contains(token, "thinking") {
		return spec.Supports(router.CapAdaptiveThinking) ||
			spec.Supports(router.CapExtendedThinking)
	}
	return true
}

func joinKept(beta string, keep func(string) bool) string {
	if beta == "" {
		return ""
	}
	parts := strings.Split(beta, ",")
	kept := parts[:0]
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t == "" || !keep(t) {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, ",")
}

func (e *RequestEnvelope) buildAnthropicFromOpenAI(opts EmitOptions) (map[string]any, error) {
	out := make(map[string]any)
	out["model"] = opts.TargetModel
	if r := gjson.GetBytes(e.body, "stream"); r.Exists() {
		out["stream"] = r.Value()
	}

	if err := e.pullOpenAIMessages(out); err != nil {
		return nil, err
	}
	pullMaxTokens(e.body, out, opts.TargetModel)
	pullStopSequences(e.body, out)
	if err := e.pullOpenAITools(out); err != nil {
		return nil, err
	}
	pullToolChoice(e.body, out)
	pullSharedParams(e.body, out)

	return out, nil
}

func (e *RequestEnvelope) pullOpenAIMessages(out map[string]any) error {
	src, err := e.ensureSrc()
	if err != nil {
		return err
	}
	msgs, ok := src["messages"].([]any)
	if !ok {
		return nil
	}
	system, translated := translateMessages(msgs)
	out["messages"] = translated
	if len(system) > 0 {
		out["system"] = system
	}
	return nil
}

func pullMaxTokens(body []byte, out map[string]any, targetModel string) {
	if r := gjson.GetBytes(body, "max_tokens"); r.Exists() {
		out["max_tokens"] = r.Value()
		return
	}
	if r := gjson.GetBytes(body, "max_completion_tokens"); r.Exists() {
		out["max_tokens"] = r.Value()
		return
	}
	out["max_tokens"] = defaultOutputTokens(targetModel)
}

func pullStopSequences(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "stop")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		out["stop_sequences"] = []string{r.String()}
		return
	}
	if r.IsArray() {
		out["stop_sequences"] = r.Value()
	}
}

func (e *RequestEnvelope) pullOpenAITools(out map[string]any) error {
	src, err := e.ensureSrc()
	if err != nil {
		return err
	}
	tools, ok := src["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	var result []any
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		result = append(result, map[string]any{
			"name":         fn["name"],
			"description":  fn["description"],
			"input_schema": fn["parameters"],
		})
	}
	out["tools"] = result
	return nil
}

func pullToolChoice(body []byte, out map[string]any) {
	r := gjson.GetBytes(body, "tool_choice")
	if !r.Exists() {
		return
	}
	if r.Type == gjson.String {
		switch r.String() {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		case "none":
			delete(out, "tools")
		}
		return
	}
	if r.IsObject() {
		name := r.Get("function.name").String()
		if name != "" {
			out["tool_choice"] = map[string]any{"type": "tool", "name": name}
		}
	}
}

func pullSharedParams(body []byte, out map[string]any) {
	for _, key := range []string{"temperature", "top_p", "top_k"} {
		if r := gjson.GetBytes(body, key); r.Exists() {
			out[key] = r.Value()
		}
	}
}

func (e *RequestEnvelope) buildAnthropicFromAnthropic(opts EmitOptions) ([]byte, error) {
	ov := resolveAnthropicOverrides(e.body, opts)
	return e.emitSameFormat(ov)
}

func translateMessages(msgs []any) (system []any, anthropic []any) {
	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system":
			system = append(system, systemBlocksFromContent(msg["content"])...)
		case "tool":
			appendToolResult(&anthropic, msg)
		case "assistant":
			anthropic = append(anthropic, translateAssistantMessage(msg))
		default:
			anthropic = append(anthropic, translateUserMessage(msg))
		}
	}
	return system, anthropic
}

func systemBlocksFromContent(content any) []any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		var blocks []any
		for _, part := range c {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := p["type"].(string); t == "text" {
				if text, _ := p["text"].(string); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			}
		}
		return blocks
	default:
		return nil
	}
}

func appendToolResult(anthropic *[]any, msg map[string]any) {
	toolCallID, _ := msg["tool_call_id"].(string)
	block := map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolCallID,
		"content":     openAIToolContentToAnthropic(msg["content"]),
	}
	if n := len(*anthropic); n > 0 {
		last, _ := (*anthropic)[n-1].(map[string]any)
		if lastRole, _ := last["role"].(string); lastRole == "user" {
			if existing, ok := last["content"].([]any); ok {
				last["content"] = append(existing, block)
				return
			}
		}
	}
	*anthropic = append(*anthropic, map[string]any{
		"role":    "user",
		"content": []any{block},
	})
}

// toolResultContent flattens OpenAI tool message content to a single string.
func toolResultContent(raw any) string {
	switch c := raw.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			if t, _ := pm["type"].(string); t == "text" {
				if text, _ := pm["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// openAIToolContentToAnthropic converts OpenAI tool message content to
// Anthropic-compatible tool_result content.
func openAIToolContentToAnthropic(raw any) any {
	switch c := raw.(type) {
	case string:
		return c
	case []any:
		blocks := make([]any, 0, len(c))
		for _, p := range c {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}
			switch t, _ := pm["type"].(string); t {
			case "text":
				if text, _ := pm["text"].(string); text != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": text})
				}
			case "image_url":
				if block := translateImageURL(pm); block != nil {
					blocks = append(blocks, block)
				}
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	default:
		return ""
	}
}

func translateAssistantMessage(msg map[string]any) map[string]any {
	toolCalls, hasToolCalls := msg["tool_calls"].([]any)
	if !hasToolCalls || len(toolCalls) == 0 {
		out := map[string]any{"role": "assistant"}
		if content, ok := msg["content"]; ok {
			out["content"] = translateContentToAnthropic(content)
		}
		return out
	}
	var blocks []any
	if text, _ := msg["content"].(string); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range toolCalls {
		call, _ := tc.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		name, _ := fn["name"].(string)
		argsStr, _ := fn["arguments"].(string)
		var input any
		if json.Unmarshal([]byte(argsStr), &input) != nil {
			input = map[string]any{}
		}
		id, _ := call["id"].(string)
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": input,
		})
	}
	return map[string]any{"role": "assistant", "content": blocks}
}

func translateUserMessage(msg map[string]any) map[string]any {
	out := map[string]any{"role": msg["role"]}
	if content, ok := msg["content"]; ok {
		out["content"] = translateContentToAnthropic(content)
	}
	return out
}

func translateContentToAnthropic(content any) any {
	parts, ok := content.([]any)
	if !ok {
		return content
	}
	var blocks []any
	for _, part := range parts {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		pType, _ := p["type"].(string)
		switch pType {
		case "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": p["text"]})
		case "image_url":
			if block := translateImageURL(p); block != nil {
				blocks = append(blocks, block)
			}
		}
	}
	if len(blocks) > 0 {
		return blocks
	}
	return content
}

func translateImageURL(part map[string]any) map[string]any {
	imgURL, _ := part["image_url"].(map[string]any)
	if imgURL == nil {
		return nil
	}
	urlStr, _ := imgURL["url"].(string)
	if urlStr == "" {
		return nil
	}
	if strings.HasPrefix(urlStr, "data:") {
		halves := strings.SplitN(urlStr, ",", 2)
		if len(halves) != 2 {
			return nil
		}
		mediaType := strings.TrimPrefix(strings.TrimSuffix(halves[0], ";base64"), "data:")
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": mediaType,
				"data":       halves[1],
			},
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  urlStr,
		},
	}
}
