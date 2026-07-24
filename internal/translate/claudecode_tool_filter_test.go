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
// coding + scheduling tools interleaved with CC-only control-plane tools.
const claudeCodeMixedToolBody = `{
	"model":"claude-opus-4-7",
	"system":"You are a helpful coding assistant.",
	"messages":[{"role":"user","content":"fix the bug"}],
	"tools":[
		{"name":"Read","description":"r","input_schema":{"type":"object"}},
		{"name":"Edit","description":"e","input_schema":{"type":"object"}},
		{"name":"Write","description":"w","input_schema":{"type":"object"}},
		{"name":"Bash","description":"b","input_schema":{"type":"object"}},
		{"name":"NotebookEdit","description":"nb","input_schema":{"type":"object"}},
		{"name":"ScheduleWakeup","description":"loop wake","input_schema":{"type":"object"}},
		{"name":"CronCreate","description":"cron","input_schema":{"type":"object"}},
		{"name":"Monitor","description":"watch","input_schema":{"type":"object"}},
		{"name":"BashOutput","description":"bg out","input_schema":{"type":"object"}},
		{"name":"KillShell","description":"bg kill","input_schema":{"type":"object"}},
		{"name":"Task","description":"sub-agent dispatch","input_schema":{"type":"object"}},
		{"name":"Agent","description":"sub-agent dispatch","input_schema":{"type":"object"}},
		{"name":"TaskCreate","description":"","input_schema":{"type":"object"}},
		{"name":"TaskUpdate","description":"","input_schema":{"type":"object"}},
		{"name":"TaskList","description":"","input_schema":{"type":"object"}},
		{"name":"EnterPlanMode","description":"","input_schema":{"type":"object"}},
		{"name":"ExitPlanMode","description":"","input_schema":{"type":"object"}},
		{"name":"UpdatePlan","description":"","input_schema":{"type":"object"}},
		{"name":"Skill","description":"","input_schema":{"type":"object"}},
		{"name":"Workflow","description":"","input_schema":{"type":"object"}},
		{"name":"AskUserQuestion","description":"","input_schema":{"type":"object"}},
		{"name":"ToolSearch","description":"","input_schema":{"type":"object"}},
		{"name":"TodoWrite","description":"","input_schema":{"type":"object"}}
	],
	"max_tokens":256
}`

// keptOnNonAnthropicDefault are tools that survive Anthropic→non-Anthropic emit
// when KeepCrossVendorOrchestrationTools is off: coding + scheduling + shell session.
var keptOnNonAnthropicDefault = []string{
	"Read", "Edit", "Write", "Bash", "NotebookEdit",
	"ScheduleWakeup", "CronCreate", "Monitor", "BashOutput", "KillShell",
}

func TestStripCCTools_AnthropicSourceOpenAITarget_DropsCCOnlyKeepsReal(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	names := emittedToolNames(t, out.Body)
	assert.ElementsMatch(t, keptOnNonAnthropicDefault, names,
		"coding + scheduling tools survive; CC-only control-plane schemas stripped on Anthropic→OpenAI")
	assert.NotContains(t, names, "Task")
	assert.NotContains(t, names, "Agent")
	assert.NotContains(t, names, "ToolSearch")
}

func TestStripCCTools_AnthropicSourceGeminiTarget_DropsCCOnlyKeepsReal(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)

	names := emittedGeminiToolNames(t, out.Body)
	assert.ElementsMatch(t, keptOnNonAnthropicDefault, names,
		"Anthropic→Gemini keeps scheduling tools and drops CC-only control-plane schemas")
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
	assert.Contains(t, got, "Agent")
	assert.Contains(t, got, "TaskUpdate")
	assert.Contains(t, got, "Skill")
	assert.Contains(t, got, "ScheduleWakeup")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "NotebookEdit")
	assert.Contains(t, got, "Read", "real coding tools must also survive on passthrough")
}

func TestKeepOrchestrationTools_OpenAITarget_KeepsOrchestrationDropsRest(t *testing.T) {
	// Flag on: orchestration tools survive; other CC-only tools (AskUserQuestion,
	// ToolSearch) are still stripped. Scheduling/NotebookEdit are not CC-only.
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:                       "deepseek/deepseek-v4-pro",
		KeepCrossVendorOrchestrationTools: true,
	})
	require.NoError(t, err)

	names := emittedToolNames(t, out.Body)
	assert.ElementsMatch(t, []string{
		"Read", "Edit", "Write", "Bash", "NotebookEdit",
		"ScheduleWakeup", "CronCreate", "Monitor", "BashOutput", "KillShell",
		"Task", "Agent", "TaskCreate", "TaskUpdate", "TaskList",
		"EnterPlanMode", "ExitPlanMode", "UpdatePlan", "Skill", "Workflow",
	}, names, "orchestration + scheduling tools survive when the flag is on")
	assert.NotContains(t, names, "AskUserQuestion", "non-orchestration CC-only tools stay stripped")
	assert.NotContains(t, names, "ToolSearch")
	assert.NotContains(t, names, "TodoWrite")
}

func TestKeepOrchestrationTools_OpenAITarget_NormalizesTypelessAnyOf(t *testing.T) {
	body := `{
		"model":"claude-opus-4-7",
		"messages":[{"role":"user","content":"run a workflow"}],
		"tools":[{
			"name":"Workflow",
			"input_schema":{"type":"object","properties":{"args":{"anyOf":[{}, {"type":"null"}]}}}
		}]
	}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{
		TargetModel:                       "deepseek/deepseek-v4-pro",
		KeepCrossVendorOrchestrationTools: true,
	})
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(out.Body, &doc))
	tools := doc["tools"].([]any)
	workflow := tools[0].(map[string]any)["function"].(map[string]any)
	params := workflow["parameters"].(map[string]any)
	args := params["properties"].(map[string]any)["args"].(map[string]any)
	branch := args["anyOf"].([]any)[0].(map[string]any)
	assert.Equal(t, []any{"string", "number", "boolean", "object", "array", "null"}, branch["type"],
		"OpenAI requires every anyOf branch to declare a type")
}

func TestKeepOrchestrationTools_GeminiTarget_KeepsOrchestrationDropsRest(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:                       "gemini-3.1-pro-preview",
		KeepCrossVendorOrchestrationTools: true,
	})
	require.NoError(t, err)

	names := emittedGeminiToolNames(t, out.Body)
	assert.ElementsMatch(t, []string{
		"Read", "Edit", "Write", "Bash", "NotebookEdit",
		"ScheduleWakeup", "CronCreate", "Monitor", "BashOutput", "KillShell",
		"Task", "Agent", "TaskCreate", "TaskUpdate", "TaskList",
		"EnterPlanMode", "ExitPlanMode", "UpdatePlan", "Skill", "Workflow",
	}, names, "Anthropic→Gemini keeps orchestration + scheduling tools when the flag is on")
	assert.NotContains(t, names, "AskUserQuestion")
	assert.NotContains(t, names, "ToolSearch")
}

func TestKeepOrchestrationTools_EmitOptionsZeroValue_StripsAll(t *testing.T) {
	// Zero-value EmitOptions strips CC-only control-plane tools (incl.
	// orchestration); production default (on) is set by the proxy composition
	// root. Scheduling + coding tools are not CC-only and always survive.
	env, err := translate.ParseAnthropic([]byte(claudeCodeMixedToolBody))
	require.NoError(t, err)

	out, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "deepseek/deepseek-v4-pro"})
	require.NoError(t, err)

	names := emittedToolNames(t, out.Body)
	assert.ElementsMatch(t, keptOnNonAnthropicDefault, names,
		"unset flag must strip orchestration CC-only tools; scheduling tools stay")
	assert.NotContains(t, names, "Task")
	assert.NotContains(t, names, "Agent")
	assert.NotContains(t, names, "Skill")
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
