package translate_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"workweave/router/internal/translate"
)

func TestGeminiToAnthropic_ParallelToolsPairByNameOrder(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "do both"}]},
			{"role": "model", "parts": [
				{"functionCall": {"name": "edit", "args": {"path": "a.go"}}},
				{"functionCall": {"name": "read", "args": {"path": "b.go"}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "edit", "response": {"result": "edited"}}},
				{"functionResponse": {"name": "read", "response": {"result": "contents"}}}
			]}
		]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)

	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 3)
	editID := msgs[1].Get("content.0.id").String()
	readID := msgs[1].Get("content.1.id").String()
	require.NotEmpty(t, editID)
	require.NotEmpty(t, readID)
	assert.NotEqual(t, editID, readID)
	assert.Equal(t, editID, msgs[2].Get("content.0.tool_use_id").String())
	assert.Equal(t, readID, msgs[2].Get("content.1.tool_use_id").String())
	assert.Equal(t, "edited", msgs[2].Get("content.0.content").String())
	assert.Equal(t, "contents", msgs[2].Get("content.1.content").String())
}

func TestGeminiToAnthropic_SnakeCaseAliases(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"text": "go"}]},
			{"role": "model", "parts": [{"function_call": {"name": "edit", "arguments": {"path": "x.go"}}}]},
			{"role": "user", "parts": [{"function_response": {"name": "edit", "response": {"output": "done"}}}]}
		]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)

	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 3)
	assert.Equal(t, "tool_use", msgs[1].Get("content.0.type").String())
	assert.Equal(t, "edit", msgs[1].Get("content.0.name").String())
	assert.Equal(t, "x.go", msgs[1].Get("content.0.input.path").String())
	toolID := msgs[1].Get("content.0.id").String()
	assert.Equal(t, "tool_result", msgs[2].Get("content.0.type").String())
	assert.Equal(t, toolID, msgs[2].Get("content.0.tool_use_id").String())
	assert.Equal(t, "done", msgs[2].Get("content.0.content").String())
}

func TestGeminiToAnthropic_OrphanFunctionResponseStillEmitsToolResult(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"contents": [
			{"role": "user", "parts": [{"functionResponse": {"name": "edit", "response": {"result": "late"}}}]}
		]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)

	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 1)
	assert.Equal(t, "tool_result", msgs[0].Get("content.0.type").String())
	assert.Contains(t, msgs[0].Get("content.0.tool_use_id").String(), "orphan")
	assert.Equal(t, "late", msgs[0].Get("content.0.content").String())
}

func TestGeminiToAnthropic_NestedResponseFallsBackToRawJSON(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"contents": [
			{"role": "model", "parts": [{"functionCall": {"name": "search", "args": {}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "search", "response": {"hits": [{"id": 1}, {"id": 2}]}}}]}
		]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)

	content := gjson.GetBytes(prep.Body, "messages.1.content.0.content").String()
	assert.Contains(t, content, `"hits"`)
	assert.Contains(t, content, `"id": 1`)
}

func TestGeminiToAnthropic_PreservesStreamFlag(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"stream": true,
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)
	assert.True(t, gjson.GetBytes(prep.Body, "stream").Bool())
}

func TestGeminiToAnthropic_DoesNotCopyGeminiToolsArray(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"tools": [{"functionDeclarations": [{"name": "edit", "parameters": {"type": "object"}}]}],
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)
	assert.False(t, gjson.GetBytes(prep.Body, "tools").Exists(), "summarizer ingest must not forward Gemini tools schemas")
}

func TestGeminiToAnthropic_SameNameToolsPairToLatestCall(t *testing.T) {
	t.Parallel()

	// Two edit calls; name→id map keeps the latest. Documented pairing rule.
	body := []byte(`{
		"contents": [
			{"role": "model", "parts": [
				{"functionCall": {"name": "edit", "args": {"n": 1}}},
				{"functionCall": {"name": "edit", "args": {"n": 2}}}
			]},
			{"role": "user", "parts": [
				{"functionResponse": {"name": "edit", "response": {"result": "first"}}},
				{"functionResponse": {"name": "edit", "response": {"result": "second"}}}
			]}
		]
	}`)
	env, err := translate.ParseGemini(body)
	require.NoError(t, err)

	prep, err := env.PrepareAnthropic(http.Header{}, translate.EmitOptions{TargetModel: "claude-haiku-4-5"})
	require.NoError(t, err)

	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 2)
	latestID := msgs[0].Get("content.1.id").String()
	assert.Equal(t, latestID, msgs[1].Get("content.0.tool_use_id").String())
	assert.Equal(t, latestID, msgs[1].Get("content.1.tool_use_id").String())
}
