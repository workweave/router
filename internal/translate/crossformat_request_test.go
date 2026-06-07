package translate_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var openAISimpleConversation = []byte(`{
	"model": "gpt-4",
	"stream": true,
	"messages": [
		{"role": "system", "content": "You are a coding assistant."},
		{"role": "user", "content": "What is 2+2?"},
		{"role": "assistant", "content": "2+2 equals 4."},
		{"role": "user", "content": "Now what about 3+3?"}
	],
	"temperature": 0.7,
	"max_tokens": 1024
}`)

var openAIToolConversation = []byte(`{
	"model": "gpt-4",
	"stream": true,
	"messages": [
		{"role": "system", "content": "You are a coding assistant."},
		{"role": "user", "content": "Read the file main.go"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_abc", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"main.go\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_abc", "content": "package main\n\nfunc main() {}"},
		{"role": "assistant", "content": "The file contains a basic main package."},
		{"role": "user", "content": "Now edit it to add a hello world print"}
	],
	"tools": [
		{"type": "function", "function": {"name": "Read", "description": "Read a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}},
		{"type": "function", "function": {"name": "Edit", "description": "Edit a file", "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "old_string": {"type": "string"}, "new_string": {"type": "string"}}, "required": ["path", "old_string", "new_string"]}}}
	],
	"max_tokens": 4096,
	"temperature": 0.7
}`)

var openAIImageConversation = []byte(`{
	"model": "gpt-4",
	"messages": [
		{"role": "user", "content": [
			{"type": "text", "text": "What is in this image?"},
			{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}
		]}
	],
	"max_tokens": 512
}`)

var openAIMultipleSystemMessages = []byte(`{
	"model": "gpt-4",
	"messages": [
		{"role": "system", "content": "You follow strict formatting rules."},
		{"role": "system", "content": "Always respond in JSON."},
		{"role": "user", "content": "Give me a greeting."}
	],
	"max_tokens": 256
}`)

var openAIAssistantTextAndToolCalls = []byte(`{
	"model": "gpt-4",
	"messages": [
		{"role": "user", "content": "Search for recent PRs"},
		{"role": "assistant", "content": "Let me search for that.", "tool_calls": [
			{"id": "call_search", "type": "function", "function": {"name": "search_prs", "arguments": "{\"query\":\"recent\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_search", "content": "Found 3 PRs"},
		{"role": "user", "content": "Tell me about PR #1"}
	],
	"tools": [
		{"type": "function", "function": {"name": "search_prs", "description": "Search PRs", "parameters": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]}}}
	],
	"max_tokens": 1024
}`)

var openAIEmptyToolArgs = []byte(`{
	"model": "gpt-4",
	"messages": [
		{"role": "user", "content": "List the files"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_ls", "type": "function", "function": {"name": "ListFiles", "arguments": "{}"}}
		]},
		{"role": "tool", "tool_call_id": "call_ls", "content": "main.go\ngo.mod"}
	],
	"tools": [
		{"type": "function", "function": {"name": "ListFiles", "description": "List files", "parameters": {"type": "object", "properties": {}}}}
	],
	"max_tokens": 512
}`)

var openAINullToolResult = []byte(`{
	"model": "gpt-4",
	"messages": [
		{"role": "user", "content": "Run the command"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_run", "type": "function", "function": {"name": "Exec", "arguments": "{\"cmd\":\"ls\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_run", "content": null}
	],
	"tools": [
		{"type": "function", "function": {"name": "Exec", "description": "Run command", "parameters": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}}
	],
	"max_tokens": 512
}`)

var anthropicSimpleConversation = []byte(`{
	"model": "claude-sonnet-4-20250514",
	"stream": true,
	"system": "You are a coding assistant.",
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "What is 2+2?"}]},
		{"role": "assistant", "content": [{"type": "text", "text": "2+2 equals 4."}]},
		{"role": "user", "content": [{"type": "text", "text": "Now what about 3+3?"}]}
	],
	"temperature": 0.7,
	"max_tokens": 1024
}`)

var anthropicToolConversation = []byte(`{
	"model": "claude-sonnet-4-20250514",
	"stream": true,
	"system": "You are a coding assistant.",
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "Read the file main.go"}]},
		{"role": "assistant", "content": [
			{"type": "text", "text": "I will read the file."},
			{"type": "tool_use", "id": "toolu_abc", "name": "Read", "input": {"path": "main.go"}}
		]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "toolu_abc", "content": "package main\n\nfunc main() {}"}
		]},
		{"role": "assistant", "content": [{"type": "text", "text": "The file contains a basic main package."}]},
		{"role": "user", "content": [{"type": "text", "text": "Now edit it to add a hello world print"}]}
	],
	"tools": [
		{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}},
		{"name": "Edit", "description": "Edit a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}, "old_string": {"type": "string"}, "new_string": {"type": "string"}}, "required": ["path", "old_string", "new_string"]}}
	],
	"max_tokens": 4096,
	"temperature": 0.7
}`)

var anthropicImageConversation = []byte(`{
	"model": "claude-sonnet-4-20250514",
	"messages": [
		{"role": "user", "content": [
			{"type": "text", "text": "What is in this image?"},
			{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo="}}
		]}
	],
	"max_tokens": 512
}`)

var anthropicArraySystemConversation = []byte(`{
	"model": "claude-sonnet-4-20250514",
	"system": [
		{"type": "text", "text": "You follow strict formatting rules."},
		{"type": "text", "text": "Always respond in JSON."}
	],
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "Give me a greeting."}]}
	],
	"max_tokens": 256
}`)

var anthropicEmptyToolArgs = []byte(`{
	"model": "claude-sonnet-4-20250514",
	"messages": [
		{"role": "user", "content": [{"type": "text", "text": "List the files"}]},
		{"role": "assistant", "content": [
			{"type": "tool_use", "id": "toolu_ls", "name": "ListFiles", "input": {}}
		]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "toolu_ls", "content": "main.go\ngo.mod"}
		]},
		{"role": "user", "content": [{"type": "text", "text": "Which is the entry point?"}]}
	],
	"tools": [
		{"name": "ListFiles", "description": "List files", "input_schema": {"type": "object", "properties": {}}}
	],
	"max_tokens": 512
}`)

func unmarshalBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func getArray(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, ok := m[key]
	require.True(t, ok, "missing key %q", key)
	arr, ok := v.([]any)
	require.True(t, ok, "key %q is not an array", key)
	return arr
}

func getMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	require.True(t, ok, "missing key %q", key)
	out, ok := v.(map[string]any)
	require.True(t, ok, "key %q is not an object, got %T", key, v)
	return out
}

func msgAt(t *testing.T, msgs []any, i int) map[string]any {
	t.Helper()
	require.Greater(t, len(msgs), i, "messages[%d] out of range", i)
	m, ok := msgs[i].(map[string]any)
	require.True(t, ok, "messages[%d] is not an object", i)
	return m
}
func TestCrossFormat_OpenAIToAnthropic_SimpleText(t *testing.T) {
	env, err := translate.ParseOpenAI(openAISimpleConversation)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	assert.Equal(t, "claude-sonnet-4-20250514", doc["model"])
	assert.Equal(t, true, doc["stream"])

	sys := getArray(t, doc, "system")
	require.Len(t, sys, 1)
	sysBlock := sys[0].(map[string]any)
	assert.Equal(t, "text", sysBlock["type"])
	assert.Equal(t, "You are a coding assistant.", sysBlock["text"])

	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 3)

	u0 := msgAt(t, msgs, 0)
	assert.Equal(t, "user", u0["role"])
	assert.Equal(t, "What is 2+2?", u0["content"])

	a1 := msgAt(t, msgs, 1)
	assert.Equal(t, "assistant", a1["role"])
	assert.Equal(t, "2+2 equals 4.", a1["content"])

	u2 := msgAt(t, msgs, 2)
	assert.Equal(t, "user", u2["role"])
	assert.Equal(t, "Now what about 3+3?", u2["content"])

	assert.Equal(t, float64(0.7), doc["temperature"])
	assert.Equal(t, float64(1024), doc["max_tokens"])
}

func TestCrossFormat_OpenAIToAnthropic_ToolConversation(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 2)
	readTool := tools[0].(map[string]any)
	assert.Equal(t, "Read", readTool["name"])
	assert.Equal(t, "Read a file", readTool["description"])
	inputSchema := getMap(t, readTool, "input_schema")
	assert.Equal(t, "object", inputSchema["type"])
	assert.NotContains(t, readTool, "function", "Anthropic tools must not have a 'function' wrapper")

	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 5)

	userMsg := msgAt(t, msgs, 0)
	assert.Equal(t, "user", userMsg["role"])

	assistantMsg := msgAt(t, msgs, 1)
	assert.Equal(t, "assistant", assistantMsg["role"])
	blocks := assistantMsg["content"].([]any)
	require.Len(t, blocks, 1)
	toolUseBlock := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", toolUseBlock["type"])
	assert.Equal(t, "call_abc", toolUseBlock["id"])
	assert.Equal(t, "Read", toolUseBlock["name"])
	input := getMap(t, toolUseBlock, "input")
	assert.Equal(t, "main.go", input["path"])

	toolResultMsg := msgAt(t, msgs, 2)
	assert.Equal(t, "user", toolResultMsg["role"])
	resultBlocks := toolResultMsg["content"].([]any)
	require.Len(t, resultBlocks, 1)
	resultBlock := resultBlocks[0].(map[string]any)
	assert.Equal(t, "tool_result", resultBlock["type"])
	assert.Equal(t, "call_abc", resultBlock["tool_use_id"])
	assert.Equal(t, "package main\n\nfunc main() {}", resultBlock["content"])

	assistantReply := msgAt(t, msgs, 3)
	assert.Equal(t, "assistant", assistantReply["role"])
	assert.Equal(t, "The file contains a basic main package.", assistantReply["content"])

	finalUser := msgAt(t, msgs, 4)
	assert.Equal(t, "user", finalUser["role"])
	assert.Equal(t, "Now edit it to add a hello world print", finalUser["content"])
}

func TestCrossFormat_OpenAIToAnthropic_Image(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIImageConversation)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 1)

	userMsg := msgAt(t, msgs, 0)
	content := userMsg["content"].([]any)
	require.Len(t, content, 2)

	textBlock := content[0].(map[string]any)
	assert.Equal(t, "text", textBlock["type"])
	assert.Equal(t, "What is in this image?", textBlock["text"])

	imgBlock := content[1].(map[string]any)
	assert.Equal(t, "image", imgBlock["type"])
	src := getMap(t, imgBlock, "source")
	assert.Equal(t, "base64", src["type"])
	assert.Equal(t, "image/png", src["media_type"])
	assert.Equal(t, "iVBORw0KGgo=", src["data"])
}

func TestCrossFormat_OpenAIToAnthropic_MultipleSystemMessages(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIMultipleSystemMessages)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	sys := getArray(t, doc, "system")
	require.Len(t, sys, 2, "both system messages must become blocks")
	assert.Equal(t, "You follow strict formatting rules.", sys[0].(map[string]any)["text"])
	assert.Equal(t, "Always respond in JSON.", sys[1].(map[string]any)["text"])

	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgAt(t, msgs, 0)["role"])
}

func TestCrossFormat_OpenAIToAnthropic_AssistantTextAndToolCalls(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIAssistantTextAndToolCalls)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	assistantMsg := msgAt(t, msgs, 1)
	assert.Equal(t, "assistant", assistantMsg["role"])
	blocks := assistantMsg["content"].([]any)
	require.Len(t, blocks, 2, "text block + tool_use block")
	textBlock := blocks[0].(map[string]any)
	assert.Equal(t, "text", textBlock["type"])
	assert.Equal(t, "Let me search for that.", textBlock["text"])
	toolBlock := blocks[1].(map[string]any)
	assert.Equal(t, "tool_use", toolBlock["type"])
	assert.Equal(t, "search_prs", toolBlock["name"])
}

func TestCrossFormat_OpenAIToAnthropic_EmptyToolArgs(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIEmptyToolArgs)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	assistantMsg := msgAt(t, msgs, 1)
	blocks := assistantMsg["content"].([]any)
	require.Len(t, blocks, 1)
	toolBlock := blocks[0].(map[string]any)
	assert.Equal(t, "tool_use", toolBlock["type"])
	input, ok := toolBlock["input"].(map[string]any)
	require.True(t, ok, "input must be a JSON object, not a string")
	assert.Empty(t, input, "empty args must produce empty input object")
}

func TestCrossFormat_OpenAIToAnthropic_NullToolResult(t *testing.T) {
	env, err := translate.ParseOpenAI(openAINullToolResult)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	toolResultMsg := msgAt(t, msgs, 2)
	blocks := toolResultMsg["content"].([]any)
	require.Len(t, blocks, 1)
	block := blocks[0].(map[string]any)
	assert.Equal(t, "tool_result", block["type"])
	assert.Equal(t, "", block["content"], "null tool content must become empty string")
}
func TestCrossFormat_OpenAIToGemini_SimpleText(t *testing.T) {
	env, err := translate.ParseOpenAI(openAISimpleConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	assert.NotContains(t, doc, "model", "Gemini body must not carry a top-level model field")
	assert.NotContains(t, doc, "stream", "Gemini body must not carry a top-level stream field")

	sys := getMap(t, doc, "systemInstruction")
	parts := sys["parts"].([]any)
	require.Len(t, parts, 1)
	assert.Equal(t, "You are a coding assistant.", parts[0].(map[string]any)["text"])

	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 3)

	u0 := msgAt(t, contents, 0)
	assert.Equal(t, "user", u0["role"])
	u0Parts := u0["parts"].([]any)
	assert.Equal(t, "What is 2+2?", u0Parts[0].(map[string]any)["text"])

	m1 := msgAt(t, contents, 1)
	assert.Equal(t, "model", m1["role"])
	m1Parts := m1["parts"].([]any)
	assert.Equal(t, "2+2 equals 4.", m1Parts[0].(map[string]any)["text"])

	u2 := msgAt(t, contents, 2)
	assert.Equal(t, "user", u2["role"])
	u2Parts := u2["parts"].([]any)
	assert.Equal(t, "Now what about 3+3?", u2Parts[0].(map[string]any)["text"])

	genConf := getMap(t, doc, "generationConfig")
	assert.Equal(t, float64(0.7), genConf["temperature"])
	assert.Equal(t, float64(1024), genConf["maxOutputTokens"])

	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader))
}

func TestCrossFormat_OpenAIToGemini_ToolConversation(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 1)
	toolWrapper := tools[0].(map[string]any)
	decls := toolWrapper["functionDeclarations"].([]any)
	require.Len(t, decls, 2)
	readDecl := decls[0].(map[string]any)
	assert.Equal(t, "Read", readDecl["name"])
	assert.Equal(t, "Read a file", readDecl["description"])
	assert.NotNil(t, readDecl["parameters"])

	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 5)

	modelFuncCall := msgAt(t, contents, 1)
	assert.Equal(t, "model", modelFuncCall["role"])
	modelParts := modelFuncCall["parts"].([]any)
	require.Len(t, modelParts, 1)
	fc := modelParts[0].(map[string]any)["functionCall"].(map[string]any)
	assert.Equal(t, "Read", fc["name"])
	args := fc["args"].(map[string]any)
	assert.Equal(t, "main.go", args["path"])

	toolResponse := msgAt(t, contents, 2)
	assert.Equal(t, "user", toolResponse["role"])
	trParts := toolResponse["parts"].([]any)
	fr := trParts[0].(map[string]any)["functionResponse"].(map[string]any)
	assert.Equal(t, "Read", fr["name"])
	result := fr["response"].(map[string]any)
	assert.Equal(t, "package main\n\nfunc main() {}", result["result"])
}

// TestCrossFormat_OpenAIToGemini_DropsSigLessToolsForGemini3x covers the
// mirror of the Anthropic→Gemini guard for the OpenAI surface. An OpenAI
// client whose assistant history was produced by a non-Gemini provider
// carries `tool_calls` without `thought_signature`. Routed to a Gemini 3.x
// preview model, the upstream rejects the request with 400 on missing
// thoughtSignature. The translator must drop the sig-less tool history.
func TestCrossFormat_OpenAIToGemini_DropsSigLessToolsForGemini3x(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")

	// No part across any turn should carry a functionCall or
	// functionResponse — they were all sig-less and would 400.
	var lastRole string
	for i, c := range contents {
		msg := c.(map[string]any)
		role, _ := msg["role"].(string)
		assert.NotEqual(t, lastRole, role,
			"contents[%d] role=%q must not match preceding turn's role — Gemini rejects non-alternating roles; placeholders should keep alternation across drops",
			i, role)
		lastRole = role
		parts, _ := msg["parts"].([]any)
		for _, p := range parts {
			pmap := p.(map[string]any)
			assert.Nil(t, pmap["functionCall"], "contents[%d] must not carry functionCall when sig-less history was dropped", i)
			assert.Nil(t, pmap["functionResponse"], "contents[%d] must not carry functionResponse when matching functionCall was dropped", i)
		}
	}
}

// Multi-tool turns produce one `role:"tool"` message per tool_call_id. When
// the sig-less drop guard fires, each tool message would naively emit its
// own user placeholder, producing consecutive `user` entries that Gemini
// 400s on. Verify a run of consecutive tool messages coalesces into a single
// placeholder so role alternation is preserved.
func TestCrossFormat_OpenAIToGemini_MultiToolDropCoalescesPlaceholders(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5",
		"messages":[
			{"role":"user","content":"do two things"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},
				{"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"path\":\"x\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_a","content":"ok-a"},
			{"role":"tool","tool_call_id":"call_b","content":"ok-b"},
			{"role":"user","content":"thanks"}
		]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")

	var lastRole string
	for i, c := range contents {
		role, _ := c.(map[string]any)["role"].(string)
		assert.NotEqual(t, lastRole, role,
			"contents[%d] role=%q must not match preceding turn's role — consecutive tool messages must coalesce into one placeholder",
			i, role)
		lastRole = role
	}
}

// Gemini 2.x accepts sig-less tool calls, so the drop guard must NOT fire
// there — same OpenAI fixture, different target model.
func TestCrossFormat_OpenAIToGemini_KeepsToolsForGemini2x(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")

	var hasFunctionCall, hasFunctionResponse bool
	for _, c := range contents {
		parts, _ := c.(map[string]any)["parts"].([]any)
		for _, p := range parts {
			pmap := p.(map[string]any)
			if pmap["functionCall"] != nil {
				hasFunctionCall = true
			}
			if pmap["functionResponse"] != nil {
				hasFunctionResponse = true
			}
		}
	}
	assert.True(t, hasFunctionCall, "Gemini 2.x must still receive functionCall parts")
	assert.True(t, hasFunctionResponse, "Gemini 2.x must still receive functionResponse parts")
}

func TestCrossFormat_OpenAIToGemini_Image(t *testing.T) {
	openAIBase64Body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "What is in this image?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}
			]}
		],
		"max_tokens": 512
	}`)

	env, err := translate.ParseOpenAI(openAIBase64Body)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 1)

	userMsg := msgAt(t, contents, 0)
	parts := userMsg["parts"].([]any)
	require.Len(t, parts, 2)

	textPart := parts[0].(map[string]any)
	assert.Equal(t, "What is in this image?", textPart["text"])

	imgPart := parts[1].(map[string]any)
	inlineData := getMap(t, imgPart, "inlineData")
	assert.Equal(t, "image/png", inlineData["mimeType"])
	assert.Equal(t, "iVBORw0KGgo=", inlineData["data"])
}

func TestCrossFormat_OpenAIToGemini_MultipleSystemMessagesConcatenated(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIMultipleSystemMessages)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	sys := getMap(t, doc, "systemInstruction")
	parts := sys["parts"].([]any)
	require.Len(t, parts, 1)
	text := parts[0].(map[string]any)["text"].(string)
	assert.Equal(t, "You follow strict formatting rules.\nAlways respond in JSON.", text)
}

func TestCrossFormat_OpenAIToGemini_EmptyToolArgs(t *testing.T) {
	env, err := translate.ParseOpenAI(openAIEmptyToolArgs)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")

	modelTurn := msgAt(t, contents, 1)
	parts := modelTurn["parts"].([]any)
	require.Len(t, parts, 1)
	fc := parts[0].(map[string]any)["functionCall"].(map[string]any)
	assert.Equal(t, "ListFiles", fc["name"])
	args, ok := fc["args"].(map[string]any)
	require.True(t, ok, "args must be an object even when empty")
	assert.Empty(t, args)
}
func TestCrossFormat_AnthropicToOpenAI_SimpleText(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSimpleConversation)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	assert.Equal(t, "gpt-4", doc["model"])
	assert.Equal(t, true, doc["stream"])

	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 4)

	sysMsg := msgAt(t, msgs, 0)
	assert.Equal(t, "system", sysMsg["role"])
	assert.Equal(t, "You are a coding assistant.", sysMsg["content"])

	u1 := msgAt(t, msgs, 1)
	assert.Equal(t, "user", u1["role"])
	assert.Equal(t, "What is 2+2?", u1["content"])

	a2 := msgAt(t, msgs, 2)
	assert.Equal(t, "assistant", a2["role"])
	assert.Equal(t, "2+2 equals 4.", a2["content"])

	u3 := msgAt(t, msgs, 3)
	assert.Equal(t, "user", u3["role"])
	assert.Equal(t, "Now what about 3+3?", u3["content"])

	assert.Equal(t, float64(0.7), doc["temperature"])
}

func TestCrossFormat_AnthropicToOpenAI_ToolConversation(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 2)
	readTool := tools[0].(map[string]any)
	assert.Equal(t, "function", readTool["type"])
	fn := getMap(t, readTool, "function")
	assert.Equal(t, "Read", fn["name"])
	assert.Equal(t, "Read a file", fn["description"])
	params := getMap(t, fn, "parameters")
	assert.Equal(t, "object", params["type"])
	assert.NotContains(t, fn, "input_schema", "OpenAI tools must use 'parameters', not 'input_schema'")

	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 6)

	assistantMsg := msgAt(t, msgs, 2)
	assert.Equal(t, "assistant", assistantMsg["role"])
	assert.Equal(t, "I will read the file.", assistantMsg["content"])
	toolCalls := assistantMsg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)
	tc := toolCalls[0].(map[string]any)
	assert.Equal(t, "toolu_abc", tc["id"])
	assert.Equal(t, "function", tc["type"])
	tcFn := getMap(t, tc, "function")
	assert.Equal(t, "Read", tcFn["name"])
	argsStr, ok := tcFn["arguments"].(string)
	require.True(t, ok, "tool call arguments must be a JSON string, not an object")
	var argsObj map[string]any
	require.NoError(t, json.Unmarshal([]byte(argsStr), &argsObj))
	assert.Equal(t, "main.go", argsObj["path"])

	toolResultMsg := msgAt(t, msgs, 3)
	assert.Equal(t, "tool", toolResultMsg["role"])
	assert.Equal(t, "toolu_abc", toolResultMsg["tool_call_id"])
	assert.Equal(t, "package main\n\nfunc main() {}", toolResultMsg["content"])
}

func TestCrossFormat_AnthropicToOpenAI_Image(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicImageConversation)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 1)

	userMsg := msgAt(t, msgs, 0)
	content := userMsg["content"].([]any)
	require.Len(t, content, 2)

	textPart := content[0].(map[string]any)
	assert.Equal(t, "text", textPart["type"])
	assert.Equal(t, "What is in this image?", textPart["text"])

	imgPart := content[1].(map[string]any)
	assert.Equal(t, "image_url", imgPart["type"])
	imageURL := getMap(t, imgPart, "image_url")
	url, ok := imageURL["url"].(string)
	require.True(t, ok)
	assert.Contains(t, url, "data:image/png;base64,")
	assert.Contains(t, url, "iVBORw0KGgo=")
}

func TestCrossFormat_AnthropicToOpenAI_ArraySystemFlattened(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicArraySystemConversation)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	sysMsg := msgAt(t, msgs, 0)
	assert.Equal(t, "system", sysMsg["role"])
	sysContent, ok := sysMsg["content"].(string)
	require.True(t, ok, "OpenAI system content must be a string")
	assert.Contains(t, sysContent, "You follow strict formatting rules.")
	assert.Contains(t, sysContent, "Always respond in JSON.")
}

func TestCrossFormat_AnthropicToOpenAI_EmptyToolInputBecomesEmptyArgsString(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicEmptyToolArgs)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	assistantMsg := msgAt(t, msgs, 1)
	toolCalls := assistantMsg["tool_calls"].([]any)
	require.Len(t, toolCalls, 1)
	tc := toolCalls[0].(map[string]any)
	fn := getMap(t, tc, "function")
	argsStr, ok := fn["arguments"].(string)
	require.True(t, ok, "arguments must be a JSON string")
	assert.Equal(t, "{}", argsStr)
}
func TestCrossFormat_AnthropicToGemini_SimpleText(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicSimpleConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	assert.NotContains(t, doc, "model")
	assert.NotContains(t, doc, "stream")

	sys := getMap(t, doc, "systemInstruction")
	parts := sys["parts"].([]any)
	assert.Equal(t, "You are a coding assistant.", parts[0].(map[string]any)["text"])

	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 3)

	assert.Equal(t, "user", msgAt(t, contents, 0)["role"])
	assert.Equal(t, "model", msgAt(t, contents, 1)["role"])
	assert.Equal(t, "user", msgAt(t, contents, 2)["role"])

	u0Parts := msgAt(t, contents, 0)["parts"].([]any)
	assert.Equal(t, "What is 2+2?", u0Parts[0].(map[string]any)["text"])

	genConf := getMap(t, doc, "generationConfig")
	assert.Equal(t, float64(0.7), genConf["temperature"])
	assert.Equal(t, float64(1024), genConf["maxOutputTokens"])

	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader))
}

func TestCrossFormat_AnthropicToGemini_ToolConversation(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicToolConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)

	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 1)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	require.Len(t, decls, 2)
	readDecl := decls[0].(map[string]any)
	assert.Equal(t, "Read", readDecl["name"])
	assert.Equal(t, "Read a file", readDecl["description"])
	params := getMap(t, readDecl, "parameters")
	assert.Equal(t, "object", params["type"])

	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 5)

	userTurn := msgAt(t, contents, 0)
	assert.Equal(t, "user", userTurn["role"])

	modelFuncCallTurn := msgAt(t, contents, 1)
	assert.Equal(t, "model", modelFuncCallTurn["role"])
	modelParts := modelFuncCallTurn["parts"].([]any)
	require.Len(t, modelParts, 2)
	textPart := modelParts[0].(map[string]any)
	assert.Equal(t, "I will read the file.", textPart["text"])
	fcPart := modelParts[1].(map[string]any)
	fc := fcPart["functionCall"].(map[string]any)
	assert.Equal(t, "Read", fc["name"])
	args := fc["args"].(map[string]any)
	assert.Equal(t, "main.go", args["path"])

	toolResponseTurn := msgAt(t, contents, 2)
	assert.Equal(t, "user", toolResponseTurn["role"])
	trParts := toolResponseTurn["parts"].([]any)
	fr := trParts[0].(map[string]any)["functionResponse"].(map[string]any)
	assert.Equal(t, "Read", fr["name"])
	result := fr["response"].(map[string]any)
	assert.Equal(t, "package main\n\nfunc main() {}", result["result"])
}

func TestCrossFormat_AnthropicToGemini_Image(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicImageConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")
	require.Len(t, contents, 1)

	userMsg := msgAt(t, contents, 0)
	parts := userMsg["parts"].([]any)
	require.Len(t, parts, 2)

	textPart := parts[0].(map[string]any)
	assert.Equal(t, "What is in this image?", textPart["text"])

	imgPart := parts[1].(map[string]any)
	inlineData := getMap(t, imgPart, "inlineData")
	assert.Equal(t, "image/png", inlineData["mimeType"])
	assert.Equal(t, "iVBORw0KGgo=", inlineData["data"])
}

func TestCrossFormat_AnthropicToGemini_ArraySystemFlattened(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicArraySystemConversation)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	sys := getMap(t, doc, "systemInstruction")
	parts := sys["parts"].([]any)
	require.Len(t, parts, 1)
	text := parts[0].(map[string]any)["text"].(string)
	assert.Contains(t, text, "You follow strict formatting rules.")
	assert.Contains(t, text, "Always respond in JSON.")
}

func TestCrossFormat_AnthropicToGemini_EmptyToolInput(t *testing.T) {
	env, err := translate.ParseAnthropic(anthropicEmptyToolArgs)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	contents := getArray(t, doc, "contents")

	modelTurn := msgAt(t, contents, 1)
	assert.Equal(t, "model", modelTurn["role"])
	modelParts := modelTurn["parts"].([]any)
	require.Len(t, modelParts, 1)
	fc := modelParts[0].(map[string]any)["functionCall"].(map[string]any)
	assert.Equal(t, "ListFiles", fc["name"])
	args, ok := fc["args"].(map[string]any)
	require.True(t, ok, "args must be a JSON object")
	assert.Empty(t, args)
}
func TestCrossFormat_GeminiToAnthropic_IsUnsupported(t *testing.T) {
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	_, err = env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	assert.Error(t, err, "Gemini→Anthropic request translation is not implemented and must return an error")
}

func TestCrossFormat_GeminiToOpenAI_IsUnsupported(t *testing.T) {
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	_, err = env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "gpt-4", TargetProvider: "openai"})
	assert.Error(t, err, "Gemini→OpenAI request translation is not implemented and must return an error")
}
func TestCrossFormat_OpenAIToAnthropic_ScalarFieldsCarriedThrough(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}],
		"temperature": 0.5,
		"top_p": 0.9,
		"max_tokens": 2048,
		"stop": ["STOP", "END"]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	assert.Equal(t, "claude-sonnet-4-20250514", doc["model"])
	assert.Equal(t, true, doc["stream"])
	assert.Equal(t, float64(0.5), doc["temperature"])
	assert.Equal(t, float64(0.9), doc["top_p"])
	assert.Equal(t, float64(2048), doc["max_tokens"])
	seqs := doc["stop_sequences"].([]any)
	assert.Equal(t, []any{"STOP", "END"}, seqs)
}

func TestCrossFormat_AnthropicToOpenAI_ScalarFieldsCarriedThrough(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"stream": true,
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"temperature": 0.3,
		"top_p": 0.8,
		"max_tokens": 512,
		"stop_sequences": ["STOP"]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	assert.Equal(t, "gpt-4", doc["model"])
	assert.Equal(t, true, doc["stream"])
	assert.Equal(t, float64(0.3), doc["temperature"])
	assert.Equal(t, float64(0.8), doc["top_p"])
	stop := doc["stop"].([]any)
	assert.Equal(t, []any{"STOP"}, stop)
}

func TestCrossFormat_OpenAIToGemini_ScalarFieldsCarriedThrough(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"stream": true,
		"messages": [{"role": "user", "content": "hi"}],
		"temperature": 0.6,
		"top_p": 0.95,
		"max_tokens": 1024
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	genConf := getMap(t, doc, "generationConfig")
	assert.Equal(t, float64(0.6), genConf["temperature"])
	assert.Equal(t, float64(0.95), genConf["topP"])
	assert.Equal(t, float64(1024), genConf["maxOutputTokens"])
	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader))
}

func TestCrossFormat_AnthropicToGemini_ScalarFieldsCarriedThrough(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"stream": true,
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"temperature": 0.4,
		"top_p": 0.85,
		"max_tokens": 768,
		"stop_sequences": ["END"]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-pro"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	genConf := getMap(t, doc, "generationConfig")
	assert.Equal(t, float64(0.4), genConf["temperature"])
	assert.Equal(t, float64(0.85), genConf["topP"])
	assert.Equal(t, float64(768), genConf["maxOutputTokens"])
	stops := genConf["stopSequences"].([]any)
	assert.Equal(t, []any{"END"}, stops)
	assert.Equal(t, "true", prep.Headers.Get(translate.GeminiStreamHintHeader))
}
func TestCrossFormat_OpenAIToAnthropic_ToolParametersBecomesInputSchema(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"type": "function", "function": {
				"name": "search",
				"description": "Search the web",
				"parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}
			}}
		],
		"max_tokens": 512
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	assert.Equal(t, "search", tool["name"])
	assert.Equal(t, "Search the web", tool["description"])
	inputSchema := getMap(t, tool, "input_schema")
	assert.Equal(t, "object", inputSchema["type"])
	assert.NotContains(t, tool, "parameters")
	assert.NotContains(t, tool, "function")
}

func TestCrossFormat_AnthropicToOpenAI_InputSchemaBecomesParameters(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"tools": [
			{"name": "search", "description": "Search the web",
			 "input_schema": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}}
		],
		"max_tokens": 512
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	tools := getArray(t, doc, "tools")
	require.Len(t, tools, 1)
	tool := tools[0].(map[string]any)
	assert.Equal(t, "function", tool["type"])
	fn := getMap(t, tool, "function")
	assert.Equal(t, "search", fn["name"])
	params := getMap(t, fn, "parameters")
	assert.Equal(t, "object", params["type"])
	assert.NotContains(t, tool, "input_schema")
}
func TestCrossFormat_OpenAIToAnthropic_ConsecutiveToolResultsMerge(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "Check weather in two cities"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_sf", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}},
				{"id": "call_ny", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"NYC\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_sf", "content": "62F fog"},
			{"role": "tool", "tool_call_id": "call_ny", "content": "85F humid"}
		],
		"max_tokens": 512
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")
	require.Len(t, msgs, 3, "user + assistant + one merged user message with both tool_results")

	toolMsg := msgAt(t, msgs, 2)
	assert.Equal(t, "user", toolMsg["role"])
	blocks := toolMsg["content"].([]any)
	require.Len(t, blocks, 2, "both results must be in one user message")

	sf := blocks[0].(map[string]any)
	assert.Equal(t, "tool_result", sf["type"])
	assert.Equal(t, "call_sf", sf["tool_use_id"])

	ny := blocks[1].(map[string]any)
	assert.Equal(t, "tool_result", ny["type"])
	assert.Equal(t, "call_ny", ny["tool_use_id"])
}

func TestCrossFormat_AnthropicToOpenAI_ToolResultSplitIntoSeparateMessages(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Check weather"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_sf", "name": "get_weather", "input": {"city": "SF"}},
				{"type": "tool_use", "id": "toolu_ny", "name": "get_weather", "input": {"city": "NYC"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_sf", "content": "62F fog"},
				{"type": "tool_result", "tool_use_id": "toolu_ny", "content": "85F humid"}
			]},
			{"role": "user", "content": [{"type": "text", "text": "Which is nicer?"}]}
		],
		"max_tokens": 512
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)

	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel:    "gpt-4",
		TargetProvider: "openai",
	})
	require.NoError(t, err)

	doc := unmarshalBody(t, prep.Body)
	msgs := getArray(t, doc, "messages")

	var toolRoleCount int
	for _, m := range msgs {
		msg := m.(map[string]any)
		if msg["role"] == "tool" {
			toolRoleCount++
		}
	}
	assert.Equal(t, 2, toolRoleCount, "each Anthropic tool_result must become a separate OpenAI role:tool message")
}

func TestCrossFormat_AnthropicToOpenAI_ToolWithoutDescription(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"name": "Bash", "input_schema": {"type": "object", "properties": {"cmd": {"type": "string"}}, "required": ["cmd"]}}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel: "gpt-4",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))

	tools, _ := doc["tools"].([]any)
	require.Len(t, tools, 1)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	require.NotNil(t, fn)
	assert.Equal(t, "Bash", fn["name"])
	if desc, hasDesc := fn["description"]; hasDesc {
		assert.Nil(t, desc, "if present, description should be null for tools without one")
	}
}

func TestCrossFormat_OpenAIToAnthropic_StopNull(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hi"}],
		"stop": null
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel: "claude-sonnet-4-20250514",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))

	_, hasStop := doc["stop_sequences"]
	assert.False(t, hasStop, "null stop value should not produce a stop_sequences field")
}

func TestCrossFormat_OpenAIToAnthropic_ToolChoiceNoneSuppressesTools(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [
			{"type": "function", "function": {"name": "Bash", "description": "Run a command", "parameters": {"type": "object"}}}
		],
		"tool_choice": "none"
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{
		TargetModel: "claude-sonnet-4-20250514",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	// Anthropic has no "none" equivalent — omitting tools is the only way to suppress.
	_, hasTools := doc["tools"]
	assert.False(t, hasTools, "tool_choice=none must suppress tools in Anthropic output")
	_, hasToolChoice := doc["tool_choice"]
	assert.False(t, hasToolChoice, "tool_choice should not appear when none")
}

func TestCrossFormat_AnthropicToGemini_NullInputNormalizedToEmptyObject(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "run it"}]},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": null}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}
			]}
		],
		"max_tokens": 1024
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel: "gemini-2.5-flash",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))

	contents, _ := doc["contents"].([]any)
	require.NotEmpty(t, contents)
	for _, c := range contents {
		content := c.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, p := range parts {
			part := p.(map[string]any)
			if fc, ok := part["functionCall"].(map[string]any); ok {
				args := fc["args"]
				require.NotNil(t, args, "null input must be normalized to empty object, not null")
				argsMap, ok := args.(map[string]any)
				require.True(t, ok, "args must be an object, got %T", args)
				assert.Empty(t, argsMap, "null input must be normalized to empty object")
			}
		}
	}
}

func TestCrossFormat_AnthropicToOpenAI_EmptyToolsArrayOmitted(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"tools": [ ],
		"max_tokens": 1024
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel: "gpt-4",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	_, hasTools := doc["tools"]
	assert.False(t, hasTools, "empty tools array must be omitted, not emitted as empty array")
}

func TestCrossFormat_OpenAIToGemini_InvalidToolArgsReturnsError(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "Bash", "arguments": "NOT VALID JSON"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "ok"}
		]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	_, err = env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel: "gemini-2.5-flash",
	})
	assert.Error(t, err, "invalid tool_call arguments should produce an error, not silently substitute {}")
}

func TestCrossFormat_AnthropicToOpenAI_EmptyToolsNoTemperatureOverride(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"tools": [],
		"max_tokens": 1024
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	// DeepSeek triggers tool-temperature-zero and system-reminder overrides.
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-chat",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	// Empty tools must not trigger DeepSeek-specific overrides.
	_, hasTemp := doc["temperature"]
	assert.False(t, hasTemp, "empty tools must not trigger temperature override")
}

func TestCrossFormat_AnthropicToOpenAI_EmptyToolsNoSystemReminder(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"system": "You are helpful.",
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]}
		],
		"tools": [],
		"max_tokens": 1024
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{
		TargetModel: "deepseek/deepseek-chat",
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	msgs := getArray(t, doc, "messages")
	for _, m := range msgs {
		msg := m.(map[string]any)
		if msg["role"] == "system" {
			content, _ := msg["content"].(string)
			assert.NotContains(t, content, "file-edit tools",
				"empty tools must not inject DeepSeek system reminder")
		}
	}
}

func TestCrossFormat_AnthropicToOpenAI_ToolUseWithMissingName(t *testing.T) {
	// tool_use block with missing "name" field — must produce valid JSON, not malformed
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_123", "input": {"x": 1}}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "gpt-4"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))
}

func TestCrossFormat_AnthropicToOpenAI_ToolResultMissingToolUseID(t *testing.T) {
	// tool_result with missing tool_use_id — must produce valid JSON
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "content": "done"}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "gpt-4"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))
}

func TestCrossFormat_OpenAIToGemini_SchemaRefsInlined(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{
			"type": "function",
			"function": {
				"name": "create_item",
				"description": "Create an item",
				"parameters": {
					"type": "object",
					"properties": {
						"item": {"$ref": "#/$defs/Item"}
					},
					"$defs": {
						"Item": {"type": "object", "properties": {"name": {"type": "string"}}}
					}
				}
			}
		}]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-flash"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	tools, _ := doc["tools"].([]any)
	require.Len(t, tools, 1)
	fds, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	require.Len(t, fds, 1)
	params, _ := fds[0].(map[string]any)["parameters"].(map[string]any)
	require.NotNil(t, params)

	_, hasDefs := params["$defs"]
	assert.False(t, hasDefs, "$defs should be inlined and removed")

	props, _ := params["properties"].(map[string]any)
	item, _ := props["item"].(map[string]any)
	require.NotNil(t, item, "item property should exist after $ref resolution")
	assert.Equal(t, "object", item["type"], "$ref should be resolved to Item schema")
	itemProps, _ := item["properties"].(map[string]any)
	assert.Contains(t, itemProps, "name", "resolved Item schema should have name property")
}

func TestCrossFormat_AnthropicToGemini_SchemaRefsInlined(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-20250514",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{
			"name": "create_item",
			"description": "Create an item",
			"input_schema": {
				"type": "object",
				"properties": {
					"item": {"$ref": "#/$defs/Item"}
				},
				"$defs": {
					"Item": {"type": "object", "properties": {"name": {"type": "string"}}}
				}
			}
		}]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-2.5-flash"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc))

	tools, _ := doc["tools"].([]any)
	require.Len(t, tools, 1)
	fds, _ := tools[0].(map[string]any)["functionDeclarations"].([]any)
	require.Len(t, fds, 1)
	params, _ := fds[0].(map[string]any)["parameters"].(map[string]any)
	require.NotNil(t, params)

	_, hasDefs := params["$defs"]
	assert.False(t, hasDefs, "$defs should be inlined and removed")

	props, _ := params["properties"].(map[string]any)
	item, _ := props["item"].(map[string]any)
	require.NotNil(t, item, "item property should exist after $ref resolution")
	assert.Equal(t, "object", item["type"])
}

func TestCrossFormat_OpenAIToAnthropic_ToolMissingFunctionName(t *testing.T) {
	// OpenAI tool with function.name absent — must produce valid JSON
	body := []byte(`{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "hi"}],
		"tools": [{"type": "function", "function": {"description": "does stuff", "parameters": {"type": "object"}}}]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &doc), "output must be valid JSON; got: %s", string(prep.Body))
}

// TestSanitizeToolUseIDs_OpenAIToAnthropic checks that tool call IDs containing
// dots or colons (e.g. Kimi-k2.6's "functions.Read:0") are sanitized before
// forwarding to Anthropic, which rejects them with pattern ^[a-zA-Z0-9_-]+$.
func TestSanitizeToolUseIDs_OpenAIToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "assistant", "content": null, "tool_calls": [
				{"id": "functions.Read:0", "type": "function", "function": {"name": "Read", "arguments": "{}"}}
			]},
			{"role": "tool", "tool_call_id": "functions.Read:0", "content": "file contents"}
		]
	}`)
	env, err := translate.ParseOpenAI(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-sonnet-4-20250514"})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	msgs := out["messages"].([]any)

	// assistant message: tool_use.id must be sanitized
	asst := msgs[0].(map[string]any)
	content := asst["content"].([]any)
	toolUse := content[0].(map[string]any)
	assert.Equal(t, "tool_use", toolUse["type"])
	assert.Equal(t, "functions_Read_0", toolUse["id"], "dots and colons must be replaced with underscores")

	// user message: tool_result.tool_use_id must match
	user := msgs[1].(map[string]any)
	userContent := user["content"].([]any)
	toolResult := userContent[0].(map[string]any)
	assert.Equal(t, "tool_result", toolResult["type"])
	assert.Equal(t, "functions_Read_0", toolResult["tool_use_id"], "tool_use_id must match the sanitized tool_use id")
}

// TestSanitizeToolUseIDs_AnthropicToAnthropic checks that the same-format path
// also sanitizes non-Anthropic tool IDs carried in Anthropic-format history.
func TestSanitizeToolUseIDs_AnthropicToAnthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "functions.Grep:11", "name": "Grep", "input": {}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "functions.Grep:11", "content": "results"}
			]}
		]
	}`)
	env, err := translate.ParseAnthropic(body)
	require.NoError(t, err)
	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)

	var out map[string]any
	require.NoError(t, json.Unmarshal(prep.Body, &out))
	msgs := out["messages"].([]any)

	asst := msgs[0].(map[string]any)
	block := asst["content"].([]any)[0].(map[string]any)
	assert.Equal(t, "functions_Grep_11", block["id"], "tool_use.id must be sanitized in same-format path")

	user := msgs[1].(map[string]any)
	result := user["content"].([]any)[0].(map[string]any)
	assert.Equal(t, "functions_Grep_11", result["tool_use_id"], "tool_use_id must be sanitized in same-format path")
}
