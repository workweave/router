package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withEscapeNormalize toggles the package-level flag for the duration of one
// subtest and restores it after.
func withEscapeNormalize(t *testing.T, enabled bool) {
	t.Helper()
	prior := translate.EnableEditEscapeNormalize
	translate.EnableEditEscapeNormalize = enabled
	t.Cleanup(func() {
		translate.EnableEditEscapeNormalize = prior
	})
}

// editToolUseResponse builds a non-streaming OpenAI response with one tool_call
// whose arguments are the provided JSON object string.
func editToolUseResponse(t *testing.T, toolName, argsJSON string) []byte {
	t.Helper()
	body := map[string]any{
		"id":    "resp_1",
		"model": "deepseek/deepseek-v4-pro",
		"choices": []any{
			map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]any{
								"name":      toolName,
								"arguments": argsJSON,
							},
						},
					},
				},
				"finish_reason": "tool_calls",
			},
		},
	}
	out, err := json.Marshal(body)
	require.NoError(t, err)
	return out
}

// firstToolUseInput pulls the `input` map of the first tool_use block from an
// Anthropic-format response body.
func firstToolUseInput(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	content, _ := doc["content"].([]any)
	for _, b := range content {
		block, _ := b.(map[string]any)
		if block == nil {
			continue
		}
		if t2, _ := block["type"].(string); t2 == "tool_use" {
			in, _ := block["input"].(map[string]any)
			return in
		}
	}
	t.Fatal("expected at least one tool_use block")
	return nil
}

func TestEscapeNormalize_FlagOff_DoesNothing(t *testing.T) {
	withEscapeNormalize(t, false)
	// JSON-encoded `\\n` lands as literal backslash-n after json.Unmarshal.
	resp := editToolUseResponse(t, "Edit", `{"file_path":"a.go","old_string":"foo\\nbar","new_string":"baz"}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
	require.NoError(t, err)

	input := firstToolUseInput(t, out)
	assert.Equal(t, `foo\nbar`, input["old_string"], "flag off must leave literal backslash-n untouched")
}

func TestEscapeNormalize_FlagOn_RewritesEditArgs(t *testing.T) {
	withEscapeNormalize(t, true)
	resp := editToolUseResponse(t, "Edit", `{"file_path":"a.go","old_string":"foo\\nbar","new_string":"baz\\tqux"}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
	require.NoError(t, err)

	input := firstToolUseInput(t, out)
	assert.Equal(t, "foo\nbar", input["old_string"], "literal backslash-n must become real newline")
	assert.Equal(t, "baz\tqux", input["new_string"], "literal backslash-t must become real tab")
}

func TestEscapeNormalize_FlagOn_LeavesRealNewlinesAlone(t *testing.T) {
	withEscapeNormalize(t, true)
	// `\n` in JSON decodes to a real newline before our code sees it; nothing to do.
	resp := editToolUseResponse(t, "Edit", `{"old_string":"foo\nbar","new_string":"baz"}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
	require.NoError(t, err)

	input := firstToolUseInput(t, out)
	assert.Equal(t, "foo\nbar", input["old_string"], "already-correct newlines pass through unchanged")
}

func TestEscapeNormalize_FlagOn_SkipsNonEditTools(t *testing.T) {
	withEscapeNormalize(t, true)
	cases := []string{"Read", "Bash", "Grep"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			resp := editToolUseResponse(t, name, `{"command":"echo foo\\nbar"}`)
			out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
			require.NoError(t, err)
			input := firstToolUseInput(t, out)
			assert.Equal(t, `echo foo\nbar`, input["command"], "non-edit tools must not be rewritten")
		})
	}
}

func TestEscapeNormalize_FlagOn_SkipsFilePath(t *testing.T) {
	withEscapeNormalize(t, true)
	resp := editToolUseResponse(t, "Edit", `{"file_path":"weird\\npath.go","old_string":"foo\\nbar"}`)

	out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
	require.NoError(t, err)

	input := firstToolUseInput(t, out)
	assert.Equal(t, `weird\npath.go`, input["file_path"], "file_path is excluded — paths can have backslashes legitimately")
	assert.Equal(t, "foo\nbar", input["old_string"], "old_string still rewritten alongside")
}

func TestEscapeNormalize_FlagOn_CaseInsensitiveToolName(t *testing.T) {
	withEscapeNormalize(t, true)
	for _, name := range []string{"edit", "EDIT", "Write", "MultiEdit"} {
		t.Run(name, func(t *testing.T) {
			resp := editToolUseResponse(t, name, `{"old_string":"x\\nx","content":"y\\ny"}`)
			out, err := translate.OpenAIToAnthropicResponse(resp, "deepseek/deepseek-v4-pro")
			require.NoError(t, err)
			input := firstToolUseInput(t, out)
			if v, ok := input["old_string"]; ok {
				assert.Equal(t, "x\nx", v)
			}
			if v, ok := input["content"]; ok {
				assert.Equal(t, "y\ny", v)
			}
		})
	}
}
