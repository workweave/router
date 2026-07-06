package proxy

import (
	"workweave/router/internal/router"
	"workweave/router/internal/translate"
)

func conversationMessagesForRouting(env *translate.RequestEnvelope) []router.ConversationMessage {
	if env == nil {
		return nil
	}
	messages := env.ConversationMessages()
	out := make([]router.ConversationMessage, 0, len(messages))
	for _, msg := range messages {
		calls := make([]router.ConversationToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			calls = append(calls, router.ConversationToolCall{
				Name:      call.Name,
				InputKeys: append([]string(nil), call.InputKeys...),
			})
		}
		results := make([]router.ConversationToolResult, 0, len(msg.ToolResults))
		for _, result := range msg.ToolResults {
			results = append(results, router.ConversationToolResult{
				ToolUseID: result.ToolUseID,
				IsError:   result.IsError,
			})
		}
		out = append(out, router.ConversationMessage{
			Role:        msg.Role,
			Text:        msg.Text,
			ToolCalls:   calls,
			ToolResults: results,
		})
	}
	return out
}

func availableToolsForRouting(env *translate.RequestEnvelope) []string {
	if env == nil {
		return nil
	}
	names := env.AvailableToolNames()
	if len(names) == 0 {
		return nil
	}
	return append([]string(nil), names...)
}
