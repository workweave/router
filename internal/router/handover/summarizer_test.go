package handover_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"workweave/router/internal/router/handover"
	"workweave/router/internal/translate"
)

// anthropicConversation is a representative 6-message conversation:
// top-level system, two user turns, two assistant turns, and a trailing
// user tool_result follow-up. RewriteEnvelope must preserve system on
// the separate top-level field and collapse messages to
// [summary, lastUser].
const anthropicConversation = `{
  "model": "claude-opus-4-7",
  "system": "You are a careful assistant.",
  "messages": [
    {"role": "user", "content": "Plan a refactor of pkg/foo."},
    {"role": "assistant", "content": "Sure — I will start with renames."},
    {"role": "user", "content": "Now apply step 1."},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "edit", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "edit applied"}]},
    {"role": "user", "content": "Continue with step 2."}
  ]
}`

func TestRewriteEnvelope_AnthropicCollapsesToSummaryPlusLastUser(t *testing.T) {
	t.Parallel()

	env, err := translate.ParseAnthropic([]byte(anthropicConversation))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "Refactor of pkg/foo in progress; step 1 applied.")

	// 6 original messages, only the trailing user is preserved → 5 elided.
	assert.Equal(t, 5, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)

	body := prep.Body
	// System stayed on the top-level field.
	assert.Equal(t, "You are a careful assistant.", gjson.GetBytes(body, "system").String())

	msgs := gjson.GetBytes(body, "messages").Array()
	require.Len(t, msgs, 2, "expect [summary, lastUser]")

	// First entry: synthesized assistant summary, tagged.
	assert.Equal(t, "assistant", msgs[0].Get("role").String())
	summaryText := msgs[0].Get("content.0.text").String()
	assert.True(t, strings.HasPrefix(summaryText, translate.HandoverSummaryTag), "summary must carry the tag prefix; got %q", summaryText)
	assert.Contains(t, summaryText, "Refactor of pkg/foo")

	// Second entry: the original trailing user message verbatim.
	assert.Equal(t, "user", msgs[1].Get("role").String())
	assert.Equal(t, "Continue with step 2.", msgs[1].Get("content").String())
}

func TestRewriteEnvelope_NilEnvelopeReturnsZeroAndDoesNotPanic(t *testing.T) {
	t.Parallel()

	require.NotPanics(t, func() {
		got := handover.RewriteEnvelope(nil, "ignored")
		assert.Equal(t, 0, got)
	})
}

func TestRewriteEnvelope_NoUserMessagesYieldsSummaryOnlyMessageList(t *testing.T) {
	t.Parallel()

	body := `{
  "model": "claude-opus-4-7",
  "system": "sys",
  "messages": [
    {"role": "assistant", "content": "hello"},
    {"role": "assistant", "content": "still talking to myself"}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "Brief recap.")

	// Both original assistant messages dropped; no user message to keep.
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 1, "expect summary-only list when no user message is present")
	assert.Equal(t, "assistant", msgs[0].Get("role").String())
}

func TestRewriteEnvelope_OpenAIPreservesLeadingSystemMessages(t *testing.T) {
	t.Parallel()

	body := `{
  "model": "gpt-5",
  "messages": [
    {"role": "system", "content": "policy A"},
    {"role": "system", "content": "policy B"},
    {"role": "user", "content": "first question"},
    {"role": "assistant", "content": "first answer"},
    {"role": "user", "content": "second question"},
    {"role": "assistant", "content": "second answer"},
    {"role": "user", "content": "final question"}
  ]
}`
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "Summarized.")

	// 5 non-system messages, latestUser preserved → 4 elided.
	assert.Equal(t, 4, elided)

	prep, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "gpt-5"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 4, "expect [system, system, summary, lastUser]")
	assert.Equal(t, "system", msgs[0].Get("role").String())
	assert.Equal(t, "system", msgs[1].Get("role").String())
	assert.Equal(t, "assistant", msgs[2].Get("role").String())
	assert.True(t, strings.HasPrefix(msgs[2].Get("content").String(), translate.HandoverSummaryTag))
	assert.Equal(t, "user", msgs[3].Get("role").String())
	assert.Equal(t, "final question", msgs[3].Get("content").String())
}

func TestTrimLastN_KeepsLastNMessagesAndSystem(t *testing.T) {
	t.Parallel()

	// Anthropic-shape conversation with 10 messages; system on the
	// top-level field stays untouched. TrimLastN(3) keeps the last 3,
	// eliding 7.
	const tenMessageBody = `{
  "model": "claude-opus-4-7",
  "system": "sys",
  "messages": [
    {"role": "user", "content": "m1"},
    {"role": "assistant", "content": "m2"},
    {"role": "user", "content": "m3"},
    {"role": "assistant", "content": "m4"},
    {"role": "user", "content": "m5"},
    {"role": "assistant", "content": "m6"},
    {"role": "user", "content": "m7"},
    {"role": "assistant", "content": "m8"},
    {"role": "user", "content": "m9"},
    {"role": "assistant", "content": "m10"}
  ]
}`

	env, err := translate.ParseAnthropic([]byte(tenMessageBody))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 3)
	assert.Equal(t, 7, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	assert.Equal(t, "sys", gjson.GetBytes(prep.Body, "system").String())
	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 3)
	assert.Equal(t, "m8", msgs[0].Get("content").String())
	assert.Equal(t, "m9", msgs[1].Get("content").String())
	assert.Equal(t, "m10", msgs[2].Get("content").String())
}

func TestTrimLastN_ZeroDefaultsToThree(t *testing.T) {
	t.Parallel()

	const body = `{
  "model": "claude-opus-4-7",
  "messages": [
    {"role": "user", "content": "m1"},
    {"role": "assistant", "content": "m2"},
    {"role": "user", "content": "m3"},
    {"role": "assistant", "content": "m4"},
    {"role": "user", "content": "m5"}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 0)
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 3, "n=0 must default to 3")
}

func TestTrimLastN_NilEnvelopeReturnsZero(t *testing.T) {
	t.Parallel()

	got := handover.TrimLastN(nil, 5)
	assert.Equal(t, 0, got)
}
