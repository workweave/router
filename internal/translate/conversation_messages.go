package translate

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type ConversationMessage struct {
	Role        string
	Text        string
	ToolCalls   []ConversationToolCall
	ToolResults []ConversationToolResult
}

type ConversationToolCall struct {
	Name      string
	InputKeys []string
}

type ConversationToolResult struct {
	ToolUseID string
	IsError   bool
}

// ConversationMessages returns provider-neutral visible message history.
func (e *RequestEnvelope) ConversationMessages() []ConversationMessage {
	if e == nil {
		return nil
	}
	switch e.format {
	case FormatAnthropic:
		return e.anthropicConversationMessages()
	case FormatOpenAI:
		return e.openAIConversationMessages()
	case FormatGemini:
		return e.geminiConversationMessages()
	default:
		return nil
	}
}

func (e *RequestEnvelope) anthropicConversationMessages() []ConversationMessage {
	out := make([]ConversationMessage, 0)
	if text := strings.TrimSpace(systemTextGJSON(gjson.GetBytes(e.body, "system"))); text != "" {
		out = append(out, ConversationMessage{Role: "system", Text: text})
	}
	gjson.GetBytes(e.body, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			return true
		}
		content := msg.Get("content")
		out = append(out, ConversationMessage{
			Role:        role,
			Text:        textForRole(role, content),
			ToolCalls:   anthropicToolCalls(content),
			ToolResults: anthropicToolResults(content),
		})
		return true
	})
	return compactConversationMessages(out)
}

func (e *RequestEnvelope) openAIConversationMessages() []ConversationMessage {
	out := make([]ConversationMessage, 0)
	gjson.GetBytes(e.body, "messages").ForEach(func(_, msg gjson.Result) bool {
		role := strings.TrimSpace(msg.Get("role").String())
		if role == "" {
			return true
		}
		out = append(out, ConversationMessage{
			Role:      role,
			Text:      strings.TrimSpace(openAIContentTextGJSON(msg.Get("content"))),
			ToolCalls: openAIToolCalls(msg.Get("tool_calls")),
		})
		return true
	})
	return compactConversationMessages(out)
}

func (e *RequestEnvelope) geminiConversationMessages() []ConversationMessage {
	out := make([]ConversationMessage, 0)
	if text := strings.TrimSpace(geminiSystemText(e.body)); text != "" {
		out = append(out, ConversationMessage{Role: "system", Text: text})
	}
	gjson.GetBytes(e.body, "contents").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		switch role {
		case "model":
			role = "assistant"
		case "":
			role = "user"
		}
		parts := msg.Get("parts")
		out = append(out, ConversationMessage{
			Role:      role,
			Text:      strings.TrimSpace(geminiPartsText(parts)),
			ToolCalls: geminiToolCalls(parts),
		})
		if toolText := strings.TrimSpace(geminiFunctionResponseText(parts)); toolText != "" {
			out = append(out, ConversationMessage{Role: "tool", Text: toolText})
		}
		return true
	})
	return compactConversationMessages(out)
}

func textForRole(role string, content gjson.Result) string {
	if role == "user" {
		return strings.TrimSpace(userPromptTextGJSON(content))
	}
	return strings.TrimSpace(contentTextGJSON(content))
}

func anthropicToolCalls(content gjson.Result) []ConversationToolCall {
	if !content.IsArray() {
		return nil
	}
	calls := make([]ConversationToolCall, 0)
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "tool_use" {
			return true
		}
		calls = append(calls, ConversationToolCall{
			Name:      strings.TrimSpace(block.Get("name").String()),
			InputKeys: objectKeys(block.Get("input")),
		})
		return true
	})
	return calls
}

func anthropicToolResults(content gjson.Result) []ConversationToolResult {
	if !content.IsArray() {
		return nil
	}
	results := make([]ConversationToolResult, 0)
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "tool_result" {
			return true
		}
		results = append(results, ConversationToolResult{
			ToolUseID: strings.TrimSpace(block.Get("tool_use_id").String()),
			IsError:   block.Get("is_error").Bool(),
		})
		return true
	})
	return results
}

func openAIToolCalls(value gjson.Result) []ConversationToolCall {
	if !value.IsArray() {
		return nil
	}
	calls := make([]ConversationToolCall, 0)
	value.ForEach(func(_, toolCall gjson.Result) bool {
		function := toolCall.Get("function")
		if !function.Exists() {
			return true
		}
		calls = append(calls, ConversationToolCall{
			Name:      strings.TrimSpace(function.Get("name").String()),
			InputKeys: jsonObjectKeys(function.Get("arguments").String()),
		})
		return true
	})
	return calls
}

func geminiToolCalls(parts gjson.Result) []ConversationToolCall {
	if !parts.IsArray() {
		return nil
	}
	calls := make([]ConversationToolCall, 0)
	parts.ForEach(func(_, part gjson.Result) bool {
		call := part.Get("functionCall")
		if !call.Exists() {
			call = part.Get("function_call")
		}
		if !call.Exists() {
			return true
		}
		args := call.Get("args")
		if !args.Exists() {
			args = call.Get("arguments")
		}
		calls = append(calls, ConversationToolCall{
			Name:      strings.TrimSpace(call.Get("name").String()),
			InputKeys: objectKeys(args),
		})
		return true
	})
	return calls
}

func geminiFunctionResponseText(parts gjson.Result) string {
	if !parts.IsArray() {
		return ""
	}
	values := make([]string, 0)
	parts.ForEach(func(_, part gjson.Result) bool {
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if !resp.Exists() {
			return true
		}
		if name := strings.TrimSpace(resp.Get("name").String()); name != "" {
			values = append(values, "Function response: "+name)
		}
		if raw := strings.TrimSpace(resp.Get("response").Raw); raw != "" {
			values = append(values, raw)
		}
		return true
	})
	return strings.Join(values, "\n")
}

func objectKeys(value gjson.Result) []string {
	if !value.IsObject() {
		return nil
	}
	keys := make([]string, 0)
	value.ForEach(func(key, _ gjson.Result) bool {
		if key.String() != "" {
			keys = append(keys, key.String())
		}
		return true
	})
	sort.Strings(keys)
	return keys
}

func jsonObjectKeys(raw string) []string {
	var object map[string]any
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func compactConversationMessages(messages []ConversationMessage) []ConversationMessage {
	out := make([]ConversationMessage, 0, len(messages))
	for _, msg := range messages {
		msg.Role = strings.TrimSpace(msg.Role)
		msg.Text = strings.TrimSpace(msg.Text)
		if msg.Role == "" || (msg.Text == "" && len(msg.ToolCalls) == 0 && len(msg.ToolResults) == 0) {
			continue
		}
		out = append(out, msg)
	}
	return out
}
