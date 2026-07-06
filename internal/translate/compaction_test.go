package translate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// anthropicToolTurn builds a user message carrying a single tool_result whose
// body is the given text, for the given tool_use_id.
func anthropicToolResultMsg(id, text string) string {
	return `{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"` + text + `"}]}`
}

func anthropicAssistantToolUse(id string) string {
	return `{"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"read","input":{}}]}`
}

func TestClearOldToolResults_Anthropic(t *testing.T) {
	body := `{"model":"claude-opus-4-8","system":"sys","messages":[` +
		anthropicAssistantToolUse("t1") + `,` + anthropicToolResultMsg("t1", "OLD_ONE") + `,` +
		anthropicAssistantToolUse("t2") + `,` + anthropicToolResultMsg("t2", "OLD_TWO") + `,` +
		anthropicAssistantToolUse("t3") + `,` + anthropicToolResultMsg("t3", "RECENT") +
		`]}`
	e, err := ParseAnthropic([]byte(body))
	require.NoError(t, err)

	cleared := e.ClearOldToolResults(1)
	assert.Equal(t, 2, cleared, "two of three tool results should be cleared")

	got := string(e.body)
	assert.Contains(t, got, ClearedToolResultPlaceholder)
	assert.NotContains(t, got, "OLD_ONE")
	assert.NotContains(t, got, "OLD_TWO")
	assert.Contains(t, got, "RECENT", "the most recent tool result must be preserved verbatim")

	// Structure intact: still 6 messages, tool_use blocks untouched.
	msgs := gjson.GetBytes(e.body, "messages").Array()
	assert.Len(t, msgs, 6)
	assert.Equal(t, 2, strings.Count(got, ClearedToolResultPlaceholder))
}

func TestClearOldToolResults_NoOpWhenWithinKeep(t *testing.T) {
	body := `{"messages":[` + anthropicAssistantToolUse("t1") + `,` + anthropicToolResultMsg("t1", "ONLY") + `]}`
	e, err := ParseAnthropic([]byte(body))
	require.NoError(t, err)
	before := string(e.body)
	assert.Equal(t, 0, e.ClearOldToolResults(5))
	assert.Equal(t, before, string(e.body), "body must be unchanged when nothing is cleared")
}

func TestClearOldToolResults_OpenAI(t *testing.T) {
	body := `{"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"OLD_OUTPUT"},` +
		`{"role":"assistant","tool_calls":[{"id":"c2","type":"function","function":{"name":"f","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c2","content":"RECENT_OUTPUT"}` +
		`]}`
	e, err := ParseOpenAI([]byte(body))
	require.NoError(t, err)

	assert.Equal(t, 1, e.ClearOldToolResults(1))
	got := string(e.body)
	assert.NotContains(t, got, "OLD_OUTPUT")
	assert.Contains(t, got, "RECENT_OUTPUT")
	assert.Contains(t, got, ClearedToolResultPlaceholder)
}

func TestRewriteForCompaction_Anthropic_KeepsSummaryAndRecent(t *testing.T) {
	// 8 alternating messages; keep recent 3 turns.
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-4-8","system":"sys","messages":[`)
	parts := []string{
		`{"role":"user","content":"u1 old"}`,
		`{"role":"assistant","content":"a1 old"}`,
		`{"role":"user","content":"u2 old"}`,
		`{"role":"assistant","content":"a2 old"}`,
		`{"role":"user","content":"u3 keep"}`,
		`{"role":"assistant","content":"a3 keep"}`,
		`{"role":"user","content":"u4 latest"}`,
	}
	b.WriteString(strings.Join(parts, ","))
	b.WriteString(`]}`)
	e, err := ParseAnthropic([]byte(b.String()))
	require.NoError(t, err)

	elided := e.RewriteForCompaction("THE SUMMARY", 3)
	assert.Positive(t, elided)

	msgs := gjson.GetBytes(e.body, "messages").Array()
	require.GreaterOrEqual(t, len(msgs), 2)
	// First message is the tagged assistant summary.
	assert.Equal(t, "assistant", msgs[0].Get("role").String())
	assert.Contains(t, msgs[0].Get("content").Array()[0].Get("text").String(), HandoverSummaryTag)
	assert.Contains(t, msgs[0].Get("content").Array()[0].Get("text").String(), "THE SUMMARY")
	// The message after the summary must be a user turn (valid alternation).
	assert.Equal(t, "user", msgs[1].Get("role").String())
	// Latest user turn preserved; oldest elided.
	got := string(e.body)
	assert.Contains(t, got, "u4 latest")
	assert.NotContains(t, got, "u1 old")
	// System field untouched.
	assert.Equal(t, "sys", gjson.GetBytes(e.body, "system").String())
}

func TestRewriteForCompaction_Anthropic_StripsOrphanedToolResult(t *testing.T) {
	// The recent window begins on a tool_result whose tool_use is elided.
	body := `{"messages":[` +
		anthropicAssistantToolUse("t1") + `,` +
		anthropicToolResultMsg("t1", "orphan-after-trim") + `,` +
		`{"role":"assistant","content":"a"},` +
		`{"role":"user","content":"latest"}` +
		`]}`
	e, err := ParseAnthropic([]byte(body))
	require.NoError(t, err)

	e.RewriteForCompaction("S", 2)
	got := string(e.body)
	// The orphaned tool_result (its tool_use t1 was elided) must not survive.
	assert.NotContains(t, got, "orphan-after-trim")
	assert.Contains(t, got, "latest")
}

func TestRewriteForCompaction_OpenAI_PreservesSystem(t *testing.T) {
	body := `{"messages":[` +
		`{"role":"system","content":"SYS"},` +
		`{"role":"user","content":"u1 old"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"user","content":"u2 latest"}` +
		`]}`
	e, err := ParseOpenAI([]byte(body))
	require.NoError(t, err)

	e.RewriteForCompaction("SUM", 1)
	msgs := gjson.GetBytes(e.body, "messages").Array()
	require.NotEmpty(t, msgs)
	assert.Equal(t, "system", msgs[0].Get("role").String())
	assert.Equal(t, "SYS", msgs[0].Get("content").String())
	got := string(e.body)
	assert.Contains(t, got, "u2 latest")
	assert.NotContains(t, got, "u1 old")
	assert.Contains(t, got, "SUM")
}

func TestRewriteForCompaction_Gemini_KeepsSummaryModelTurn(t *testing.T) {
	body := `{"contents":[` +
		`{"role":"user","parts":[{"text":"u1 old"}]},` +
		`{"role":"model","parts":[{"text":"m1"}]},` +
		`{"role":"user","parts":[{"text":"u2 latest"}]}` +
		`]}`
	e, err := ParseGemini([]byte(body))
	require.NoError(t, err)

	e.RewriteForCompaction("GSUM", 1)
	contents := gjson.GetBytes(e.body, "contents").Array()
	require.GreaterOrEqual(t, len(contents), 2)
	assert.Equal(t, "model", contents[0].Get("role").String())
	assert.Contains(t, contents[0].Get("parts").Array()[0].Get("text").String(), "GSUM")
	assert.Equal(t, "user", contents[1].Get("role").String())
	assert.Contains(t, string(e.body), "u2 latest")
}
