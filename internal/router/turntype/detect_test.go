package turntype_test

import (
	"testing"

	"workweave/router/internal/router/turntype"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetectFromEnvelope_Anthropic(t *testing.T) {
	tests := []struct {
		name string
		body string
		hint string
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
			// Regression guard: Claude Code's main-loop system prompt embeds
			// the Agent tool description, which contains "subagent_type" as
			// a parameter name. That used to trigger SubAgentDispatch and
			// hard-pin every turn to the cheap model. Detection now requires
			// the metadata.user_id or x-weave-subagent-type signal.
			name: "main_loop with Agent tool description mentioning subagent_type stays main_loop",
			body: `{"model":"claude-opus-4-7","system":"Use the Agent tool with a subagent_type parameter to dispatch sub-agents.","messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "sub-agent via x-weave-subagent-type header hint",
			body: `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"grep"}]}`,
			hint: "Explore",
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
			name: "max_tokens=1 quota probe is probe",
			body: `{"model":"claude-haiku-4-5","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
			want: turntype.Probe,
		},
		{
			name: "max_tokens=4 still probe (SDK variation buffer)",
			body: `{"model":"claude-haiku-4-5","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.Probe,
		},
		{
			name: "max_tokens=5 is not a probe",
			body: `{"model":"claude-haiku-4-5","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "no max_tokens is not a probe",
			body: `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello there"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "probe takes priority over compaction system prompt",
			body: `{"model":"claude-sonnet-4-5","max_tokens":1,"system":"Your task is to create a detailed summary","messages":[{"role":"user","content":"quota"}]}`,
			want: turntype.Probe,
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
			env, err := translate.ParseAnthropic([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			got := turntype.DetectFromEnvelope(env, feats, tc.hint)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDetectFromEnvelope_OpenAI(t *testing.T) {
	tests := []struct {
		name string
		body string
		hint string
		want turntype.TurnType
	}{
		{
			name: "regular user prompt is main_loop",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"explain recursion"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "trailing tool message is tool_result",
			body: `{"model":"gpt-4o","messages":[
				{"role":"user","content":"run the grep"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t1","content":"file.go:12: foo"}
			]}`,
			want: turntype.ToolResult,
		},
		{
			name: "tool followed by user message is main_loop",
			body: `{"model":"gpt-4o","messages":[
				{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t1","content":"out"},
				{"role":"user","content":"keep going"}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "compaction via system message",
			body: `{"model":"gpt-4o","messages":[
				{"role":"system","content":"Your task is to create a detailed summary of the conversation so far."},
				{"role":"user","content":"go"}
			]}`,
			want: turntype.Compaction,
		},
		{
			name: "sub-agent via header hint",
			body: `{"model":"gpt-4o","messages":[{"role":"user","content":"grep"}]}`,
			hint: "Explore",
			want: turntype.SubAgentDispatch,
		},
		{
			// Regression guard — see TestDetectFromEnvelope_Anthropic.
			name: "system message mentioning subagent_type stays main_loop",
			body: `{"model":"gpt-4o","messages":[
				{"role":"system","content":"Use the Agent tool with a subagent_type parameter."},
				{"role":"user","content":"list files"}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "last message assistant is main_loop",
			body: `{"model":"gpt-4o","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"hello"}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "max_completion_tokens=1 is probe (modern OpenAI SDK)",
			body: `{"model":"gpt-4o","max_completion_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
			want: turntype.Probe,
		},
		{
			name: "max_tokens=1 is probe (legacy OpenAI SDK)",
			body: `{"model":"gpt-4o","max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`,
			want: turntype.Probe,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseOpenAI([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			got := turntype.DetectFromEnvelope(env, feats, tc.hint)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDetectFromEnvelope_Gemini(t *testing.T) {
	tests := []struct {
		name string
		body string
		hint string
		want turntype.TurnType
	}{
		{
			name: "regular user prompt is main_loop",
			body: `{"contents":[{"role":"user","parts":[{"text":"explain recursion"}]}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "trailing functionResponse is tool_result",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"run grep"}]},
				{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"out":"x"}}}]}
			]}`,
			want: turntype.ToolResult,
		},
		{
			name: "compaction via systemInstruction",
			body: `{
				"systemInstruction":{"parts":[{"text":"Your task is to create a detailed summary of the conversation so far."}]},
				"contents":[{"role":"user","parts":[{"text":"go"}]}]
			}`,
			want: turntype.Compaction,
		},
		{
			name: "sub-agent via header hint",
			body: `{"contents":[{"role":"user","parts":[{"text":"grep"}]}]}`,
			hint: "Explore",
			want: turntype.SubAgentDispatch,
		},
		{
			// Regression guard — see TestDetectFromEnvelope_Anthropic.
			name: "systemInstruction mentioning subagent_type stays main_loop",
			body: `{
				"systemInstruction":{"parts":[{"text":"Use the Agent tool with a subagent_type parameter."}]},
				"contents":[{"role":"user","parts":[{"text":"list files"}]}]
			}`,
			want: turntype.MainLoop,
		},
		{
			name: "trailing model message is main_loop",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"hi"}]},
				{"role":"model","parts":[{"text":"hello"}]}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "generationConfig.maxOutputTokens=1 is probe",
			body: `{"generationConfig":{"maxOutputTokens":1},"contents":[{"role":"user","parts":[{"text":"quota"}]}]}`,
			want: turntype.Probe,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseGemini([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			got := turntype.DetectFromEnvelope(env, feats, tc.hint)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestDetectFromEnvelope_NilEnv(t *testing.T) {
	got := turntype.DetectFromEnvelope(nil, translate.RoutingFeatures{}, "")
	assert.Equal(t, turntype.MainLoop, got)
}
