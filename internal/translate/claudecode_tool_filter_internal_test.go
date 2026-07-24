package translate

import "testing"

// claudeCodeOrchestrationToolNames must be a strict subset of claudeCodeOnlyToolNames;
// otherwise shouldStripCCTool silently skips the keep-orchestration branch for any
// name missing from the CC-only set.
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
		{"Read", false, false},           // real tool: never stripped
		{"Read", true, false},            // real tool: never stripped
		{"NotebookEdit", false, false},   // coding tool: never stripped
		{"ScheduleWakeup", false, false}, // scheduling: never stripped
		{"CronCreate", false, false},     // scheduling: never stripped
		{"Monitor", true, false},         // scheduling: never stripped
		{"Task", false, true},            // orchestration: stripped when flag off
		{"Task", true, false},            // orchestration: kept when flag on
		{"Agent", true, false},           // orchestration (current CC name): kept when flag on
		{"Workflow", true, false},        // orchestration: kept when flag on
		{"ExitPlanMode", true, false},    // orchestration: kept when flag on
		{"AskUserQuestion", true, true},  // CC-only non-orchestration: stripped even when flag on
		{"ToolSearch", true, true},       // CC-only non-orchestration: stripped even when flag on
		{"TodoWrite", false, true},       // CC-only non-orchestration: stripped
	}
	for _, tc := range cases {
		if got := shouldStripCCTool(tc.name, tc.keepOrchestration); got != tc.want {
			t.Errorf("shouldStripCCTool(%q, keep=%v) = %v, want %v", tc.name, tc.keepOrchestration, got, tc.want)
		}
	}
}
