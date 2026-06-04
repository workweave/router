package translate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageTailPreview_Anthropic(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"system": "You are Claude.",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "hi there"}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "read file foo"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "ok reading"},
				{"type": "tool_use", "id": "tool_1", "name": "Read", "input": {"file_path": "/foo.go"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_1", "content": "file contents"}
			]}
		]
	}`)

	env, err := ParseAnthropic(body)
	require.NoError(t, err)

	msgs := env.MessageTailPreview(2, 100)
	require.Len(t, msgs, 2, "should return last 2 messages")

	assert.Equal(t, "assistant", msgs[0].Role)
	assert.Len(t, msgs[0].Blocks, 2)
	assert.Equal(t, "text", msgs[0].Blocks[0].Type)
	assert.Equal(t, "ok reading", msgs[0].Blocks[0].Preview)
	assert.Equal(t, "tool_use", msgs[0].Blocks[1].Type)
	assert.Equal(t, "Read", msgs[0].Blocks[1].Name)

	assert.Equal(t, "user", msgs[1].Role)
	assert.Len(t, msgs[1].Blocks, 1)
	assert.Equal(t, "tool_result", msgs[1].Blocks[0].Type)
	assert.Equal(t, "tool_1", msgs[1].Blocks[0].Name)
	assert.Equal(t, "file contents", msgs[1].Blocks[0].Preview)
}

func TestMessageTailPreview_OpenAI(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi there"},
			{"role": "user", "content": "read file foo"},
			{"role": "assistant", "content": "ok", "tool_calls": [
				{"type": "function", "id": "call_1", "function": {"name": "Read", "arguments": "{\"file_path\": \"/foo.go\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "file contents"}
		]
	}`)

	env, err := ParseOpenAI(body)
	require.NoError(t, err)

	msgs := env.MessageTailPreview(2, 100)
	require.Len(t, msgs, 2)

	assert.Equal(t, "assistant", msgs[0].Role)
	assert.Len(t, msgs[0].Blocks, 2)
	assert.Equal(t, "text", msgs[0].Blocks[0].Type)
	assert.Equal(t, "ok", msgs[0].Blocks[0].Preview)
	assert.Equal(t, "tool_use", msgs[0].Blocks[1].Type)
	assert.Equal(t, "Read", msgs[0].Blocks[1].Name)

	assert.Equal(t, "tool", msgs[1].Role)
	assert.Len(t, msgs[1].Blocks, 1)
	assert.Equal(t, "tool_result", msgs[1].Blocks[0].Type)
	assert.Equal(t, "call_1", msgs[1].Blocks[0].Name)
	assert.Equal(t, "file contents", msgs[1].Blocks[0].Preview)
}

func TestMessageTailPreview_Gemini(t *testing.T) {
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"contents": [
			{"role": "user", "parts": [{"text": "hello"}]},
			{"role": "model", "parts": [
				{"text": "ok reading"},
				{"functionCall": {"name": "Read", "args": {"file_path": "/foo.go"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "Read", "response": {"result": "file contents"}}}
			]}
		]
	}`)

	env, err := ParseGemini(body)
	require.NoError(t, err)

	msgs := env.MessageTailPreview(2, 100)
	require.Len(t, msgs, 2)

	assert.Equal(t, "model", msgs[0].Role)
	assert.Len(t, msgs[0].Blocks, 2)
	assert.Equal(t, "text", msgs[0].Blocks[0].Type)
	assert.Equal(t, "ok reading", msgs[0].Blocks[0].Preview)
	assert.Equal(t, "tool_use", msgs[0].Blocks[1].Type)
	assert.Equal(t, "Read", msgs[0].Blocks[1].Name)

	assert.Equal(t, "user", msgs[1].Role)
	assert.Len(t, msgs[1].Blocks, 1)
	assert.Equal(t, "tool_result", msgs[1].Blocks[0].Type)
	assert.Equal(t, "Read", msgs[1].Blocks[0].Name)
	assert.JSONEq(t, `{"result":"file contents"}`, msgs[1].Blocks[0].Preview)
}

func TestSystemTextTail(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"system": "You are Claude. You can use tools.",
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	env, err := ParseAnthropic(body)
	require.NoError(t, err)

	length, head, tail := env.SystemTextTail(15)
	fullText := "You are Claude. You can use tools."
	assert.Equal(t, len(fullText), length)
	assert.Equal(t, "You are Claude.", head)
	assert.Equal(t, " can use tools.", tail) // last 15 chars
}

func TestSystemTextTail_Short(t *testing.T) {
	body := []byte(`{
		"model": "claude-opus-4-7",
		"system": "Hi",
		"messages": [{"role": "user", "content": "hi"}]
	}`)

	env, err := ParseAnthropic(body)
	require.NoError(t, err)

	length, head, tail := env.SystemTextTail(100)
	assert.Equal(t, 2, length)
	assert.Equal(t, "Hi", head)
	assert.Equal(t, "", tail)
}
