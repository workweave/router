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
			// Claude Code's Agent tool wraps the dispatched prompt in a
			// <transcript> envelope. metadata.user_id is unset (Claude
			// Code reuses the parent session_id), so the transcript
			// marker is the only signal we get.
			name: "sub-agent via <transcript> envelope in user message",
			body: `{"model":"claude-opus-4-7","messages":[{"role":"user","content":"<transcript>\nUser: Find all router cost references\n</transcript>"}]}`,
			want: turntype.SubAgentDispatch,
		},
		{
			// Regression guard: a long main-loop user prompt that
			// happens to contain "<transcript>" deep in its body must
			// not trip the heuristic. Detection sniffs a 64-byte prefix.
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
			// max_tokens=5 is above the Probe threshold (which is the
			// canonical SDK quota-check shape at max_tokens=1). With no
			// tools and a short message list it falls into the broader
			// Classifier shape — see the security-monitor case below for
			// the canonical example.
			name: "max_tokens=5 with no tools is classifier (above probe threshold)",
			body: `{"model":"claude-haiku-4-5","max_tokens":5,"messages":[{"role":"user","content":"hello"}]}`,
			want: turntype.Classifier,
		},
		{
			// Belt for the test above: max_tokens=5 with the full tool
			// registry stays MainLoop. Probe vs Classifier boundaries
			// must never catch a real conversation that requested a
			// terse reply.
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
			// Claude Code sidebar title-gen call: tools=[] +
			// output_config.format.type=json_schema declaring a string
			// "title" property. Both structural signals required.
			name: "claude code title-gen is title_gen",
			body: `{"model":"claude-opus-4-7","max_tokens":64000,"tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}}},"messages":[{"role":"user","content":"what good cuh"}]}`,
			want: turntype.TitleGen,
		},
		{
			// Regression guard: a real conversation that happens to
			// request a title-shaped structured output but ALSO carries
			// the tool registry must NOT be hard-pinned.
			name: "title schema with tools stays main_loop",
			body: `{"model":"claude-opus-4-7","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"title":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: a structured-output call with a different
			// schema (no string "title" property) must NOT be hard-pinned.
			// Only the title-gen-shaped schema qualifies.
			name: "structured output without title field stays main_loop",
			body: `{"model":"claude-opus-4-7","tools":[],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"summary":{"type":"string"}}}}},"messages":[{"role":"user","content":"hi"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: a request without any output_config does
			// not classify as title-gen, even when tools=[].
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
		{
			// Anthropic Claude Code security monitor — the canonical
			// classifier shape: small max_tokens, no tools, short message
			// list. Must hard-pin to the cheap model so the main-loop
			// session pin isn't poisoned with a classifier verdict.
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
			// Regression guard: a real Claude Code main-loop turn always
			// ships the full tool registry, even when the user requests a
			// terse reply. tools=[non-empty] must keep it MainLoop.
			name: "short reply with tools stays main_loop",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"yes or no?"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: max_tokens above the classifier threshold
			// must keep the turn on the cluster scorer. 4096 is a typical
			// real-conversation cap.
			name: "max_tokens 4096 with no tools is not classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":4096,"messages":[{"role":"user","content":"explain this"}]}`,
			want: turntype.MainLoop,
		},
		{
			// Regression guard: a long classifier session (message_count
			// exceeding classifierMaxMessageCount) is no longer the
			// classifier shape — likely a stateful conversation. Drop
			// to MainLoop so the cluster scorer handles it.
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
			// Regression guard: classifier must not pre-empt the more
			// specific compaction match (its system-prompt fingerprint
			// is unambiguous even when max_tokens is small).
			name: "compaction takes priority over classifier",
			body: `{"model":"claude-opus-4-7","max_tokens":64,"system":"Your task is to create a detailed summary","messages":[{"role":"user","content":"go"}]}`,
			want: turntype.Compaction,
		},
		{
			// Regression guard: a short sub-agent dispatch must remain
			// SubAgentDispatch (its hard-pin is gated on the Explore
			// feature flag; classifier's is unconditional).
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
