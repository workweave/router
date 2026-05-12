package translate_test

import (
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGemini_ValidatesObject(t *testing.T) {
	t.Run("valid object", func(t *testing.T) {
		env, err := translate.ParseGemini([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
		require.NoError(t, err)
		assert.Equal(t, translate.FormatGemini, env.SourceFormat())
	})
	t.Run("not an object", func(t *testing.T) {
		_, err := translate.ParseGemini([]byte(`[]`))
		assert.ErrorIs(t, err, translate.ErrNotJSONObject)
	})
}

func TestRequestEnvelope_SystemText_Gemini(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "parts array",
			body: `{"systemInstruction":{"parts":[{"text":"alpha"},{"text":"beta"}]},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "alpha\nbeta",
		},
		{
			name: "single text shorthand",
			body: `{"systemInstruction":{"text":"alpha"},"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "alpha",
		},
		{
			name: "absent",
			body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseGemini([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.SystemText())
		})
	}
}

func TestRequestEnvelope_LastUserMessage_Gemini(t *testing.T) {
	cases := []struct {
		name string
		body string
		want translate.LastUserMessageInfo
	}{
		{
			name: "single user text",
			body: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "hello"},
		},
		{
			name: "user with functionResponse only",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"out":"x"}}}]}
			]}`,
			want: translate.LastUserMessageInfo{HasToolResult: true, ToolResultCount: 1},
		},
		{
			name: "mixed text + functionResponse",
			body: `{"contents":[
				{"role":"user","parts":[
					{"functionResponse":{"name":"Bash","response":{"out":"x"}}},
					{"text":"more please"}
				]}
			]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "more please", HasToolResult: true, ToolResultCount: 1},
		},
		{
			name: "trailing model message ignored",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"first"}]},
				{"role":"model","parts":[{"text":"reply"}]}
			]}`,
			want: translate.LastUserMessageInfo{HasText: true, Text: "first"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseGemini([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.want, env.LastUserMessage())
		})
	}
}

func TestRequestEnvelope_RoutingFeatures_Gemini_LastKind(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantKind string
	}{
		{
			name:     "user role",
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			wantKind: "user_prompt",
		},
		{
			name: "trailing functionResponse",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"go"}]},
				{"role":"model","parts":[{"functionCall":{"name":"Bash","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"name":"Bash","response":{"out":"x"}}}]}
			]}`,
			wantKind: "tool_result",
		},
		{
			name: "trailing model message",
			body: `{"contents":[
				{"role":"user","parts":[{"text":"hi"}]},
				{"role":"model","parts":[{"text":"hello"}]}
			]}`,
			wantKind: "assistant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, err := translate.ParseGemini([]byte(tc.body))
			require.NoError(t, err)
			feats := env.RoutingFeatures(false)
			assert.Equal(t, tc.wantKind, feats.LastKind)
		})
	}
}

func TestRequestEnvelope_FirstUserMessageText_Gemini(t *testing.T) {
	body := `{"contents":[
		{"role":"user","parts":[{"text":"original prompt"}]},
		{"role":"model","parts":[{"text":"reply"}]},
		{"role":"user","parts":[{"text":"second"}]}
	]}`
	env, err := translate.ParseGemini([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, "original prompt", env.FirstUserMessageText())
}

func TestPrepareGemini_SameFormatStripsSyntheticFields(t *testing.T) {
	// The handler injects "model" and "stream" for format-neutral accessors;
	// PrepareGemini must strip both before forwarding upstream.
	body := `{
		"model":"gemini-1.5-pro",
		"stream":true,
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`
	env, err := translate.ParseGemini([]byte(body))
	require.NoError(t, err)
	prep, err := env.PrepareGemini(nil, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)
	assert.NotContains(t, string(prep.Body), `"model"`,
		"synthetic model field must be stripped before forwarding")
	assert.NotContains(t, string(prep.Body), `"stream"`,
		"synthetic stream field must be stripped before forwarding")
	assert.Contains(t, string(prep.Body), `"contents"`,
		"the contents array must remain in the forwarded body")
	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader),
		"the stream hint must be carried as a synthetic header for the upstream adapter")
}
