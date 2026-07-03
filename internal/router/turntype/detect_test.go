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
			// Claude Code wraps tool_results in <system-reminder> text blocks;
			// still a tool-flow continuation, switching models would orphan the tool_use.
			name: "tool_result mixed with text is still tool_result",
			body: `{"model":"claude-sonnet-4-5","messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t1","content":"output"},
					{"type":"text","text":"<system-reminder>note</system-reminder>"}
				]}
			]}`,
			want: turntype.ToolResult,
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
			// Loose "compact"+"conversation" pair dropped: false-positived on
			// Codex/generic system prompts. Only the canonical phrase counts.
			name: "loose compact+conversation phrase no longer triggers compaction",
			body: `{"model":"claude-sonnet-4-5","system":"Please compact the conversation history into a concise summary.","messages":[{"role":"user","content":"ok"}]}`,
			want: turntype.MainLoop,
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
			// Regression guard: the main-loop system prompt's Agent tool
			// description contains "subagent_type" as a param name; that used
			// to false-trigger SubAgentDispatch. Requires metadata.user_id or
			// x-weave-subagent-type now.
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
			// Agent tool wraps dispatched prompts in <transcript>; metadata.user_id
			// is unset (parent session_id is reused), so this is the only signal.
			name: "sub-agent via <transcript> envelope in user message",
			body: `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"<transcript>\nUser: Find all router cost references\n</transcript>"}]}`,
			want: turntype.SubAgentDispatch,
		},
		{
			// Regression guard: detection only sniffs a 64-byte prefix, so a
			// "<transcript>" mention deep in a long prompt must not trip it.
			name: "long main_loop with embedded <transcript> deep in body stays main_loop",
			body: `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"please review this code and tell me what you think about its overall structure and design including a <transcript> tag we discussed earlier"}]}`,
			want: turntype.MainLoop,
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
			// Above the Probe threshold (max_tokens=1); no tools + short
			// messages falls into the Classifier shape (see security-monitor case below).
			name: "max_tokens=5 with no tools is classifier (above probe threshold)",
			body: `{"model":"claude-haiku-4-5","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`,
			want: turntype.Classifier,
		},
		{
			// Probe/Classifier boundaries must never catch a real conversation
			// that just requested a terse reply.
			name: "max_tokens=5 with tools stays main_loop",
			body: `{"model":"claude-haiku-4-5","max_tokens":5,"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "no max_tokens is not a probe",
			body: `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hello there"}]}`,
			want: turntype.MainLoop,
		},
		{
			// title-gen requires both: tools=[] and a json_schema output_config
			// declaring a string "title" property.
			name: "claude code title-gen is title_gen",
			body: `{"model":"claude-opus-4-7","max_tokens":64000,"tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}},"messages":[{"role":"user","content":"what good cuh"}]}`,
			want: turntype.TitleGen,
		},
		{
			// Regression guard: title-shaped structured output + tool registry
			// present must NOT be hard-pinned.
			name: "title schema with tools stays main_loop",
			body: `{"model":"claude-opus-4-7","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: only the title-gen-shaped schema qualifies.
			name: "structured output without title field stays main_loop",
			body: `{"model":"claude-opus-4-7","tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"summary":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "no structured output declaration stays main_loop",
			body: `{"model":"claude-opus-4-7","tools":[],"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			name: "title-gen takes priority over compaction phrase in system",
			body: `{"model":"claude-opus-4-7","tools":[],"system":"Your task is to create a detailed summary","output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.TitleGen,
		},
		{
			name: "probe takes priority over title-gen schema",
			body: `{"model":"claude-opus-4-7","max_tokens":1,"tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.Probe,
		},
		{
			name: "probe takes priority over compaction system prompt",
			body: `{"model":"claude-sonnet-4-5","max_tokens":1,"system":"Your task is to create a detailed summary","messages":[{"role":"user","content":"quota"}]}`,
			want: turntype.Probe,
		},
		{
			name: "compaction takes priority over tool_result",
			body: `{"model":"claude-sonnet-4-5","system":"Your task is to create a detailed summary of the conversation so far.","messages":[
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
		{
			// Canonical classifier shape: small max_tokens, no tools, short
			// messages. Must hard-pin cheap so it doesn't poison the main-loop pin.
			name: "anthropic security monitor classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"messages":[{"role":"user","content":"is this safe?"}],"system":"You are a security monitor for autonomous AI coding agents."}`,
			want: turntype.Classifier,
		},
		{
			name: "classifier with two messages still classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"messages":[
				{"role":"user","content":"transcript"},
				{"role":"assistant","content":"verdict?"}
			]}`,
			want: turntype.Classifier,
		},
		{
			// Regression guard: a real main-loop turn always ships the tool
			// registry, even for a terse reply request.
			name: "short reply with tools stays main_loop",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"yes or no?"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: above the classifier threshold, stays on the
			// cluster scorer (4096 is a typical real-conversation cap).
			name: "max_tokens 4096 with no tools is not classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":4096,"messages":[{"role":"user","content":"explain this"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: message_count above classifierMaxMessageCount
			// means a stateful conversation, not a classifier call.
			name: "small max_tokens but message_count 4 is main_loop",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"messages":[
				{"role":"user","content":"a"},
				{"role":"assistant","content":"b"},
				{"role":"user","content":"c"},
				{"role":"assistant","content":"d"}
			]}`,
			want: turntype.MainLoop,
		},
		{
			name: "probe takes priority over classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`,
			want: turntype.Probe,
		},
		{
			// Regression guard: compaction's system-prompt fingerprint is
			// unambiguous, so it must win even when max_tokens is small.
			name: "compaction takes priority over classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"system":"Your task is to create a detailed summary","messages":[{"role":"user","content":"go"}]}`,
			want: turntype.Compaction,
		},
		{
			// Regression guard: subagent's hard-pin is gated on the Explore
			// flag, classifier's isn't — subagent must still win.
			name: "subagent dispatch takes priority over classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"metadata":{"user_id":"subagent:Explore"},"messages":[{"role":"user","content":"grep"}]}`,
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
			// Compaction detection is gated on Anthropic wire format; Codex
			// system prompts with similar phrasing must stay main_loop.
			name: "claude-code-style summary phrase in OpenAI body stays main_loop",
			body: `{"model":"gpt-4o","messages":[
				{"role":"system","content":"Your task is to create a detailed summary of the conversation so far."},
				{"role":"user","content":"go"}
			]}`,
			want: turntype.MainLoop,
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
			// Mirror of the Anthropic mixed-content case: a functionResponse
			// alongside text is still a tool-flow continuation.
			name: "functionResponse mixed with text is still tool_result",
			body: `{"contents":[
				{"role":"user","parts":[
					{"functionResponse":{"name":"Bash","response":{"out":"x"}}},
					{"text":"note"}
				]}
			]}`,
			want: turntype.ToolResult,
		},
		{
			// Gated on Anthropic wire format; see the OpenAI parallel above.
			name: "claude-code-style summary phrase in Gemini body stays main_loop",
			body: `{
				"systemInstruction":{"parts":[{"text":"Your task is to create a detailed summary of the conversation so far."}]},
				"contents":[{"role":"user","parts":[{"text":"go"}]}]
			}`,
			want: turntype.MainLoop,
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
