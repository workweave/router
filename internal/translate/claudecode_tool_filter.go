package translate

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeCodeOnlyToolNames is the set of tools Claude Code (the client)
// implements internally — Task subagent dispatch, plan-mode toggles, Skill
// invocation, etc. They have no corresponding server-side behavior in any
// upstream provider, so emitting their schemas to non-Anthropic models is
// pure noise; worse, non-Anthropic models routinely hallucinate calls to
// them. On the v0.57 SWE-bench Verified eval, 224 phantom tool_use blocks
// for these names were observed across 150 router shards routed to
// non-Anthropic upstreams — 96% of them in the Task* family — with 27%
// clustering on the empty-patch failure subset.
//
// Anthropic models recognize these as native sub-tools and dispatch them
// correctly via the client; non-Anthropic models cannot. The filter applies
// only on Anthropic→non-Anthropic emit paths (buildOpenAIFromAnthropic and
// the Anthropic case of PrepareGemini). The Anthropic→Anthropic passthrough
// preserves them.
var claudeCodeOnlyToolNames = map[string]struct{}{
	"Task":                 {},
	"TaskCreate":           {},
	"TaskUpdate":           {},
	"TaskGet":              {},
	"TaskList":             {},
	"TaskOutput":           {},
	"TaskStop":             {},
	"EnterPlanMode":        {},
	"ExitPlanMode":         {},
	"Skill":                {},
	"Workflow":             {},
	"AskUserQuestion":      {},
	"CronCreate":           {},
	"CronDelete":           {},
	"CronList":             {},
	"Monitor":              {},
	"PushNotification":     {},
	"RemoteTrigger":        {},
	"EnterWorktree":        {},
	"ExitWorktree":         {},
	"LSP":                  {},
	"ListMcpResourcesTool": {},
	"ReadMcpResourceTool":  {},
	"NotebookEdit":         {},
}

// isClaudeCodeOnlyTool reports whether name is one of the tools Claude Code
// dispatches internally and that should not be forwarded to non-Anthropic
// upstreams. Names are compared case-sensitively because Claude Code emits
// them in PascalCase verbatim.
func isClaudeCodeOnlyTool(name string) bool {
	_, ok := claudeCodeOnlyToolNames[name]
	return ok
}

// claudeCodeOrchestrationToolNames is the subset of claudeCodeOnlyToolNames
// that drives multi-step work the client executes on the model's behalf:
// subagent dispatch (Task*), workflow runs, skill invocation, and plan-mode
// toggles. Unlike the rest of the CC-only set (Cron*, Monitor, MCP-resource
// tools, worktree/LSP helpers), a capable non-Anthropic model can emit a
// well-formed call to one of these and have the client action it — so they are
// optionally preserved on cross-vendor emit to let workflows/subagents run off
// the Anthropic family. Must stay a strict subset of claudeCodeOnlyToolNames.
var claudeCodeOrchestrationToolNames = map[string]struct{}{
	"Task":          {},
	"TaskCreate":    {},
	"TaskUpdate":    {},
	"TaskGet":       {},
	"TaskList":      {},
	"TaskOutput":    {},
	"TaskStop":      {},
	"Workflow":      {},
	"Skill":         {},
	"EnterPlanMode": {},
	"ExitPlanMode":  {},
}

// isCrossVendorOrchestrationTool reports whether name is a Claude Code
// orchestration tool that may be preserved on cross-vendor emit.
func isCrossVendorOrchestrationTool(name string) bool {
	_, ok := claudeCodeOrchestrationToolNames[name]
	return ok
}

// shouldStripCCTool reports whether a tool must be dropped from a cross-vendor
// emit. Non-CC-only tools are always kept. CC-only tools are dropped, except
// that orchestration tools are retained when keepOrchestration is set.
func shouldStripCCTool(name string, keepOrchestration bool) bool {
	if !isClaudeCodeOnlyTool(name) {
		return false
	}
	if keepOrchestration && isCrossVendorOrchestrationTool(name) {
		return false
	}
	return true
}

// filterClaudeCodeOnlyToolsFromAnthropicBody returns body with any
// Claude-Code-only tools removed from the top-level "tools" array. Returns
// body unchanged when none match, so callers can apply this unconditionally
// without paying a re-serialize cost on the common case.
//
// When keepOrchestration is set, the orchestration subset (Task*, Workflow,
// Skill, plan-mode) is retained so capable non-Anthropic models can drive
// subagents/workflows/skills; the remaining CC-only tools are still dropped.
//
// Only the tools array is rewritten; tool_choice and message content are
// left alone. tool_choice is rare and Anthropic only honors "any"/"auto"/
// name=X anyway, so a stale tool_choice referencing a stripped CC-only name
// would be ignored upstream. Message content (existing tool_use/tool_result
// blocks from past turns) is not rewritten because those represent history
// the model has already acted on — rewriting it would invalidate prompt
// caches and could leave dangling tool_use_id references.
func filterClaudeCodeOnlyToolsFromAnthropicBody(body []byte, keepOrchestration bool) (out []byte, removed int, err error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, 0, nil
	}

	tools.ForEach(func(_, t gjson.Result) bool {
		if shouldStripCCTool(t.Get("name").String(), keepOrchestration) {
			removed++
		}
		return true
	})
	if removed == 0 {
		return body, 0, nil
	}

	jw := newJSONWriter()
	jw.Arr()
	tools.ForEach(func(_, t gjson.Result) bool {
		if !shouldStripCCTool(t.Get("name").String(), keepOrchestration) {
			jw.Raw(t.Raw)
		}
		return true
	})
	jw.EndArr()
	out, err = sjson.SetRawBytes(body, "tools", jw.Bytes())
	return out, removed, err
}
