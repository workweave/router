package turntype_test

import (
	"testing"

	"workweave/router/internal/router/turntype"

	"github.com/stretchr/testify/assert"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		body string
		want turntype.TurnType
	}{
		{
			name: "regular user prompt is main_loop",
			body: `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"explain recursion"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "tool_result only last message",
			body: `{"model":"claude-sonnet-4-5","messages":[
				{"role":"user","content":"run the grep"},
				{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"file.go:12: foo"}]}
			]}`,
			want: turntype.ToolResult,
		},
		{
			name: "tool_result with text block is main_loop",
			body: `{"model":"claude-sonnet-4-5","messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t1","content":"output"},
					{"type":"text","text":"did you see this?"}
				]}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "last message is assistant — main_loop",
			body: `{"model":"claude-sonnet-4-5","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":[{"type":"text","text":"hello"}]}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "last message string content (not array) is main_loop",
			body: `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "compaction via task summary phrase",
			body: `{"model":"claude-sonnet-4-5","system":"Your task is to create a detailed summary of the conversation so far.","messages":[{"role":"user","content":"go"}]}`,
			want: turntype.Compaction,
		},
		{
			name: "compaction via compact+conversation keywords",
			body: `{"model":"claude-sonnet-4-5","system":"Please compact the conversation history into a concise summary.","messages":[{"role":"user","content":"ok"}]}`,
			want: turntype.Compaction,
		},
		{
			name: "compaction with array system prompt",
			body: `{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"Your task is to create a detailed summary of the conversation above."}],"messages":[{"role":"user","content":"go"}]}`,
			want: turntype.Compaction,
		},
		{
			name: "sub-agent via metadata user_id prefix",
			body: `{"model":"claude-haiku-4-5","metadata":{"user_id":"subagent:Explore"},"messages":[{"role":"user","content":"find all go files"}]}`,
			want: turntype.SubAgentDispatch,
		},
		{
			name: "sub-agent via system prompt sub-agent marker",
			body: `{"model":"claude-haiku-4-5","system":"You are a sub-agent for the Explore task.","messages":[{"role":"user","content":"list files"}]}`,
			want: turntype.SubAgentDispatch,
		},
		{
			name: "sub-agent via system prompt subagent_type marker",
			body: `{"model":"claude-haiku-4-5","system":"subagent_type: Explore","messages":[{"role":"user","content":"grep"}]}`,
			want: turntype.SubAgentDispatch,
		},
		{
			name: "empty body is main_loop",
			body: `{}`,
			want: turntype.MainLoop,
		},
		{
			name: "empty messages array is main_loop",
			body: `{"model":"claude-sonnet-4-5","messages":[]}`,
			want: turntype.MainLoop,
		},
		{
			name: "compaction takes priority over tool_result",
			body: `{"model":"claude-sonnet-4-5","system":"compact the conversation","messages":[
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"output"}]}
			]}`,
			want: turntype.Compaction,
		},
		{
			name: "sub-agent takes priority over tool_result",
			body: `{"model":"claude-haiku-4-5","metadata":{"user_id":"subagent:Explore"},"messages":[
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"output"}]}
			]}`,
			want: turntype.SubAgentDispatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := turntype.Detect([]byte(tc.body))
			assert.Equal(t, tc.want, got)
		})
	}
}
