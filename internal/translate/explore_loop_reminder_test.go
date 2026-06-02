package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// helper: build an Anthropic Messages body with N assistant turns each
// containing one tool_use block. The first len(names) turns use the names
// listed; remaining turns repeat the last name.
func anthropicBodyWithToolHistory(t *testing.T, names ...string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-4-8","max_tokens":1024,"messages":[`)
	b.WriteString(`{"role":"user","content":"fix the bug"}`)
	for i, name := range names {
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"toolu_`)
		b.WriteString(strings.Repeat("a", i+1))
		b.WriteString(`","name":"`)
		b.WriteString(name)
		b.WriteString(`","input":{}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_`)
		b.WriteString(strings.Repeat("a", i+1))
		b.WriteString(`","content":"ok"}]}`)
	}
	b.WriteString(`]}`)
	out := []byte(b.String())
	require.True(t, gjson.ValidBytes(out), "test fixture must be valid JSON")
	return out
}

func TestApplyExploreLoopReminder_FiresAtThreshold(t *testing.T) {
	// 11 Reads, 0 Edits — over the 10-Read threshold.
	body := anthropicBodyWithToolHistory(t,
		"Read", "Read", "Read", "Grep", "Read", "Read", "Read", "Glob", "Read", "Read", "Read")
	out, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.True(t, fired, "11 explore tool_uses with zero edits must fire the reminder")
	system := gjson.GetBytes(out, "system").String()
	assert.Contains(t, system, "exploration alone will not solve it",
		"reminder text must land in the system field")
}

func TestApplyExploreLoopReminder_SkipsBelowThreshold(t *testing.T) {
	// 9 Reads, under the 10 threshold.
	body := anthropicBodyWithToolHistory(t,
		"Read", "Read", "Read", "Grep", "Read", "Read", "Read", "Glob", "Read")
	_, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.False(t, fired, "under threshold — reminder must not fire yet")
}

func TestApplyExploreLoopReminder_SkipsAfterEditFires(t *testing.T) {
	// 12 Reads but one Edit also present — model already crossed into edit mode.
	body := anthropicBodyWithToolHistory(t,
		"Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Edit",
		"Read", "Read", "Read")
	_, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.False(t, fired,
		"a single Edit anywhere in history must suppress the reminder")
}

func TestApplyExploreLoopReminder_BashIsNotCountedAsExplore(t *testing.T) {
	// 12 Bash + zero Reads — Bash is ambiguous (could be edits via sed/echo),
	// so the detector must NOT count it as explore.
	body := anthropicBodyWithToolHistory(t,
		"Bash", "Bash", "Bash", "Bash", "Bash", "Bash",
		"Bash", "Bash", "Bash", "Bash", "Bash", "Bash")
	_, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.False(t, fired, "Bash must not count as explore — could be edits via sed/echo")
}

func TestApplyExploreLoopReminder_AppendsToStringSystem(t *testing.T) {
	body := anthropicBodyWithToolHistory(t,
		"Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read")
	body = []byte(strings.Replace(string(body), `"messages":[`,
		`"system":"You are a coding agent.","messages":[`, 1))
	out, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.True(t, fired)
	system := gjson.GetBytes(out, "system").String()
	assert.True(t, strings.HasPrefix(system, "You are a coding agent."),
		"existing string system content must be preserved as a prefix")
	assert.Contains(t, system, "exploration alone will not solve it")
}

func TestApplyExploreLoopReminder_AppendsToArraySystem(t *testing.T) {
	body := anthropicBodyWithToolHistory(t,
		"Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read", "Read")
	body = []byte(strings.Replace(string(body), `"messages":[`,
		`"system":[{"type":"text","text":"You are a coding agent."}],"messages":[`, 1))
	out, fired, err := applyExploreLoopReminderToAnthropicBody(body)
	require.NoError(t, err)
	assert.True(t, fired)
	system := gjson.GetBytes(out, "system")
	require.True(t, system.IsArray(), "array shape must be preserved")
	parts := system.Array()
	require.Len(t, parts, 2, "original block plus appended reminder")
	assert.Contains(t, parts[1].Get("text").String(), "exploration alone will not solve it")
}
