package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestEnvelope_SystemText_Anthropic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string system",
			body: `{"system":"You are helpful.","messages":[{"role":"user","content":"hi"}]}`,
			want: "You are helpful.",
		},
		{
			name: "array system blocks",
			body: `{"system":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}],"messages":[{"role":"user","content":"hi"}]}`,
			want: "alpha\nbeta",
		},
		{
			name: "missing system",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.SystemText())
		})
	}
}

func TestRequestEnvelope_SystemText_OpenAI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "single system message string content",
			body: `{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"}]}`,
			want: "You are helpful.",
		},
		{
			name: "multiple system messages concatenate",
			body: `{"messages":[{"role":"system","content":"first"},{"role":"system","content":"second"},{"role":"user","content":"hi"}]}`,
			want: "first\nsecond",
		},
		{
			name: "system message array content",
			body: `{"messages":[{"role":"system","content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]},{"role":"user","content":"hi"}]}`,
			want: "alpha\nbeta",
		},
		{
			name: "no system message",
			body: `{"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseOpenAI([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.SystemText())
		})
	}
}

func TestRequestEnvelope_LastUserMessage_Anthropic(t *testing.T) {
	cases := []struct {
		name string
		body string
		want translate.LastUserMessageInfo
	}{
		{
			name: "string content",
			body: `{"messages":[{"role":"user","content":"hello"}]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "hello"},
		},
		{
			name: "tool_result only",
			body: `{"messages":[
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"out"}]}
			]}`,
			want: translate.LastUserMessageInfo{HasToolResult: true, ToolResultCount: 1},
		},
		{
			name: "mixed text + tool_result",
			body: `{"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t1","content":"out"},
					{"type":"text","text":"more please"}
				]}
			]}`,
			want: translate.LastUserMessageInfo{
				HasText: true, Text: "more please", HasToolResult: true, ToolResultCount: 1,
			},
		},
		{
			name: "two tool_results",
			body: `{"messages":[
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"t1","content":"a"},
					{"type":"tool_result","tool_use_id":"t2","content":"b"}
				]}
			]}`,
			want: translate.LastUserMessageInfo{HasToolResult: true, ToolResultCount: 2},
		},
		{
			name: "no user message",
			body: `{"messages":[{"role":"assistant","content":"hi"}]}`,
			want: translate.LastUserMessageInfo{},
		},
		{
			name: "earlier user with assistant after",
			body: `{"messages":[
				{"role":"user","content":"first"},
				{"role":"assistant","content":"reply"}
			]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "first"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseAnthropic([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.LastUserMessage())
		})
	}
}

func TestRequestEnvelope_LastUserMessage_OpenAI(t *testing.T) {
	cases := []struct {
		name string
		body string
		want translate.LastUserMessageInfo
	}{
		{
			name: "trailing user message",
			body: `{"messages":[{"role":"user","content":"hello"}]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "hello"},
		},
		{
			name: "trailing tool message",
			body: `{"messages":[
				{"role":"user","content":"go"},
				{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t1","content":"out"}
			]}`,
			want: translate.LastUserMessageInfo{HasToolResult: true, ToolResultCount: 1},
		},
		{
			name: "two trailing tool messages",
			body: `{"messages":[
				{"role":"assistant","content":null,"tool_calls":[{"id":"t1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"t1","content":"a"},
				{"role":"tool","tool_call_id":"t2","content":"b"}
			]}`,
			want: translate.LastUserMessageInfo{HasToolResult: true, ToolResultCount: 2},
		},
		{
			name: "tool then user",
			body: `{"messages":[
				{"role":"tool","tool_call_id":"t1","content":"a"},
				{"role":"user","content":"continue"}
			]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "continue"},
		},
		{
			name: "trailing assistant",
			body: `{"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"hello"}
			]}`,
			want: translate.LastUserMessageInfo{},
		},
		{
			name: "empty messages",
			body: `{"messages":[]}`,
			want: translate.LastUserMessageInfo{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseOpenAI([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.LastUserMessage())
		})
	}
}

func TestRequestEnvelope_RoutingFeatures_OpenAI_LastKind(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantKind    string
		wantPreview string
	}{
		{
			name:        "user role",
			body:        `{"messages":[{"role":"user","content":"hi there"}]}`,
			wantKind:    "user_prompt",
			wantPreview: "hi there",
		},
		{
			name:     "tool role",
			body:     `{"messages":[{"role":"user","content":"go"},{"role":"tool","tool_call_id":"t1","content":"output"}]}`,
			wantKind: "tool_result",
		},
		{
			name:     "assistant role",
			body:     `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`,
			wantKind: "assistant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseOpenAI([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			assert.Equal(t, tc.wantKind, feats.LastKind)
			if tc.wantPreview != "" {
				assert.Equal(t, tc.wantPreview, feats.LastPreview)
			}
		})
	}
}
