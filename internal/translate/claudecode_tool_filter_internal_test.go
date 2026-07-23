package translate

import "testing"

// The orchestration set must stay a strict subset of the CC-only set: shouldStripCCTool
// only reaches its keep-orchestration branch for names it already treats as CC-only, so
// an orchestration entry that isn't also CC-only would be a silent no-op (and a lie in
// the docs). This breaks the moment someone adds an orchestration name without adding it
// to claudeCodeOnlyToolNames.
func TestOrchestrationToolsAreSubsetOfCCOnly(t *testing.T) {
	for name := range claudeCodeOrchestrationToolNames {
		if !isClaudeCodeOnlyTool(name) {
			t.Errorf("orchestration tool %q is not in claudeCodeOnlyToolNames; the subset invariant is broken", name)
		}
	}
}

func TestShouldStripCCTool(t *testing.T) {
	cases := []struct {
		name              string
		keepOrchestration bool
		want              bool
	}{
		{"Read", false, false},          // real tool: never stripped
		{"Read", true, false},           // real tool: never stripped
		{"Task", false, true},           // orchestration: stripped when flag off
		{"Task", true, false},           // orchestration: kept when flag on
		{"Workflow", true, false},       // orchestration: kept when flag on
		{"ExitPlanMode", true, false},   // orchestration: kept when flag on
		{"AskUserQuestion", true, true}, // CC-only non-orchestration: stripped even when flag on
		{"NotebookEdit", true, true},    // CC-only non-orchestration: stripped even when flag on
		{"CronCreate", false, true},     // CC-only non-orchestration: stripped
	}
	for _, tc := range cases {
		if got := shouldStripCCTool(tc.name, tc.keepOrchestration); got != tc.want {
			t.Errorf("shouldStripCCTool(%q, keep=%v) = %v, want %v", tc.name, tc.keepOrchestration, got, tc.want)
		}
	}
}
