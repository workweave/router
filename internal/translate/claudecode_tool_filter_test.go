package translate_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"workweave/router/internal/translate"
)

// On the v0.57 SWE-bench Verified router eval, 224 phantom CC-only tool_use
// blocks (96% in the Task* family) were emitted by non-Anthropic upstreams
// — Gemini 3.x, gpt-5.5, etc. — because the request body still carried
// schemas for tools the model has no business invoking (Task subagent
// dispatch, plan-mode toggles, Skill calls, etc.). These tests pin the
// filter that drops those schemas on Anthropic→non-Anthropic emit paths
// while leaving the Anthropic→Anthropic passthrough untouched.

// emittedToolNames extracts the top-level OpenAI tool names from an emitted
// chat-completions body.
func emittedToolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	raw, _ := doc["tools"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		tool, _ := r.(map[string]any)
		if tool == nil {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// emittedGeminiToolNames extracts function-declaration names from an emitted
// Gemini request body.
func emittedGeminiToolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	tools, _ := doc["tools"].([]any)
	var out []string
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			continue
		}
		decls, _ := tool["functionDeclarations"].([]any)
		for _, d := range decls {
			decl, _ := d.(map[string]any)
			if decl == nil {
				continue
			}
			if name, _ := decl["name"].(string); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// claudeCodeMixedToolBody is a representative Anthropic request body carrying
// the real coding tools Claude Code uses (Read/Edit/Write/Bash) interleaved
// with the Claude-Code-only tools that have no upstream behavior.
const claudeCodeMixedToolBody = `{
	"model":"claude-opus-4-7",
	"system":"You are a helpful coding assistant.",
	"messages":[{"role":"user","content":"fix the bug"}],
	"tools":[
		{"name":"Read","description":"r","input_schema":{"type":"object"}},
		{"name":"Edit","description":"e","input_schema":{"type":"object"}},
		{"name":"Write","description":"w","input_schema":{"type":"object"}},
		{"name":"Bash","description":"b","input_schema":{"type":"object"}},
		{"name":"Task","description":"sub-agent dispatch","input_schema":{"type":"object"}},
		{"name":"TaskCreate","description":"","input_schema":{"type":"object"}},
		{"name":"TaskUpdate","description":"","input_schema":{"type":"object"}},
		{"name":"TaskList","description":"","input_schema":{"type":"object"}},
		{"name":"EnterPlanMode","description":"","input_schema":{"type":"object"}},
		{"name":"ExitPlanMode","description":"","input_schema":{"type":"object"}},
		{"name":"Skill","description":"","input_schema":{"type":"object"}},
		{"name":"Workflow","description":"","input_schema":{"type":"object"}},
		{"name":"AskUserQuestion","description":"","input_schema":{"type":"object"}},
		{"name":"NotebookEdit","description":"","input_schema":{"type":"object"}}
	],
	"max_tokens":256
}`

func TestStripCCTools_AnthropicSourceOpenAITarget_DropsCCOnlyKeepsReal(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	names := emittedToolNames(t, out.Body)
	assert.ElementsMatch(t, []string{"Read", "Edit", "Write", "Bash"}, names,
		"only real coding tools survive; every CC-only schema must be stripped on the Anthropic→OpenAI emit path")
}

func TestStripCCTools_AnthropicSourceGeminiTarget_DropsCCOnlyKeepsReal(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	names := emittedGeminiToolNames(t, out.Body)
	assert.ElementsMatch(t, []string{"Read", "Edit", "Write", "Bash"}, names,
		"Anthropic→Gemini emit path must drop CC-only schemas before functionDeclarations")
}

func TestStripCCTools_AnthropicSourceAnthropicTarget_KeepsCCOnly(t *testing.T) {
	// Anthropic models DO know how to dispatch Task/Skill/etc. via the
	// client. The Anthropic→Anthropic passthrough must not strip them.
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Body, &doc))
	tools, _ := doc["tools"].([]any)
	var got []string
	for _, t := range tools {
		tool, _ := t.(map[string]any)
		if tool == nil {
			continue
		}
		if name, _ := tool["name"].(string); name != "" {
			got = append(got, name)
		}
	}
	assert.Contains(t, got, "Task", "Anthropic passthrough must keep Task — only the cross-provider emit strips it")
	assert.Contains(t, got, "TaskUpdate")
	assert.Contains(t, got, "Skill")
	assert.Contains(t, got, "Read", "real coding tools must also survive on passthrough")
}

func TestStripCCTools_NoCCToolsNoRewrite(t *testing.T) {
	// When the request carries no CC-only tools, the filter must not
	// re-serialize the body (cheap fast path).
	src := []byte(`{
		"model":"claude-opus-4-7",
		"system":"sys",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"Read","description":"r","input_schema":{"type":"object"}},
			{"name":"Bash","description":"b","input_schema":{"type":"object"}}
		],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"Read", "Bash"}, emittedToolNames(t, out.Body))
}

func TestStripCCTools_NoToolsAtAll(t *testing.T) {
	// Requests without a tools field must pass through unchanged — the
	// filter is a no-op on the most common shape.
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Body, &doc))
	_, has := doc["tools"]
	assert.False(t, has, "no tools field on input must mean no tools field on output")
}

func TestStripCCTools_AllToolsCCOnly_DropsToEmpty(t *testing.T) {
	// Degenerate case: every tool is CC-only. Filter returns an empty
	// tools array. Downstream emit must treat this as no-tools (the
	// reminder gate uses hasNonEmptyTools which checks length).
	src := []byte(`{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"name":"Task","input_schema":{"type":"object"}},
			{"name":"Skill","input_schema":{"type":"object"}}
		],
		"max_tokens":256
	}`)
	env, err := translate.ParseAnthropic(src)
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	assert.Empty(t, emittedToolNames(t, out.Body))
}
