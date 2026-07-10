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
	InputJSON string
}

type ConversationToolResult struct {
	ToolUseID string
	IsError   bool
	Text      string
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
		if role == "tool" || role == "function" {
			out = append(out, ConversationMessage{
				Role:        "user",
				ToolResults: openAIToolResults(msg),
			})
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
			Role:        role,
			Text:        strings.TrimSpace(geminiPartsText(parts)),
			ToolCalls:   geminiToolCalls(parts),
			ToolResults: geminiToolResults(parts),
		})
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
		name := strings.TrimSpace(block.Get("name").String())
		if name == "" {
			return true
		}
		calls = append(calls, ConversationToolCall{
			Name:      name,
			InputKeys: objectKeys(block.Get("input")),
			InputJSON: strings.TrimSpace(block.Get("input").Raw),
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
			Text:      strings.TrimSpace(contentTextGJSON(block.Get("content"))),
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
		name := strings.TrimSpace(function.Get("name").String())
		if name == "" {
			return true
		}
		calls = append(calls, ConversationToolCall{
			Name:      name,
			InputKeys: jsonObjectKeys(function.Get("arguments").String()),
			InputJSON: strings.TrimSpace(function.Get("arguments").String()),
		})
		return true
	})
	return calls
}

func openAIToolResults(msg gjson.Result) []ConversationToolResult {
	result := ConversationToolResult{
		ToolUseID: strings.TrimSpace(msg.Get("tool_call_id").String()),
		IsError:   msg.Get("is_error").Bool() || strings.EqualFold(strings.TrimSpace(msg.Get("status").String()), "error"),
		Text:      strings.TrimSpace(openAIContentTextGJSON(msg.Get("content"))),
	}
	return []ConversationToolResult{result}
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
		name := strings.TrimSpace(call.Get("name").String())
		if name == "" {
			return true
		}
		calls = append(calls, ConversationToolCall{
			Name:      name,
			InputKeys: objectKeys(args),
			InputJSON: strings.TrimSpace(args.Raw),
		})
		return true
	})
	return calls
}

func geminiToolResults(parts gjson.Result) []ConversationToolResult {
	if !parts.IsArray() {
		return nil
	}
	results := make([]ConversationToolResult, 0)
	parts.ForEach(func(_, part gjson.Result) bool {
		resp := part.Get("functionResponse")
		if !resp.Exists() {
			resp = part.Get("function_response")
		}
		if !resp.Exists() {
			return true
		}
		results = append(results, ConversationToolResult{
			ToolUseID: strings.TrimSpace(resp.Get("name").String()),
			Text:      strings.TrimSpace(geminiFunctionResponseText(resp)),
		})
		return true
	})
	return results
}

func geminiFunctionResponseText(resp gjson.Result) string {
	response := resp.Get("response")
	if !response.Exists() {
		return ""
	}
	if result := response.Get("result"); result.Exists() {
		if result.Type == gjson.String {
			return result.String()
		}
		return result.Raw
	}
	if output := response.Get("output"); output.Exists() {
		if output.Type == gjson.String {
			return output.String()
		}
		return output.Raw
	}
	return response.Raw
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
