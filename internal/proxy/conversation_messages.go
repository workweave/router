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
		out = append(out, router.ConversationMessage{
			Role:      msg.Role,
			Text:      msg.Text,
			ToolCalls: calls,
		})
	}
	return out
}
