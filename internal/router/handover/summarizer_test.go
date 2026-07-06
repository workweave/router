package handover_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"workweave/router/internal/router"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/translate"
)

// anthropicConversation: system + 2 user + 2 assistant turns + trailing
// tool_result. RewriteEnvelope must collapse messages to [summary, lastUser].
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

	// 10 messages; TrimLastN(3) keeps the last 3, eliding 7.
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

func TestTrimLastN_StripsOrphanedAnthropicToolResults(t *testing.T) {
	t.Parallel()

	// TrimLastN(3) keeps the last 3, starting with a tool_result whose
	// matching tool_use gets trimmed away.
	const body = `{
  "model": "claude-opus-4-7",
  "messages": [
    {"role": "user", "content": "hello"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "bash", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}]},
    {"role": "assistant", "content": "done"},
    {"role": "user", "content": "next question"}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 3)
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()

	// The orphaned tool_result user message should be stripped entirely,
	// leaving only [assistant "done", user "next question"].
	require.Len(t, msgs, 2)
	assert.Equal(t, "assistant", msgs[0].Get("role").String())
	assert.Equal(t, "done", msgs[0].Get("content").String())
	assert.Equal(t, "user", msgs[1].Get("role").String())
	assert.Equal(t, "next question", msgs[1].Get("content").String())
}

func TestTrimLastN_PreservesMatchedToolResults(t *testing.T) {
	t.Parallel()

	// Last 3 messages include both tool_use and tool_result — should be preserved.
	const body = `{
  "model": "claude-opus-4-7",
  "messages": [
    {"role": "user", "content": "start"},
    {"role": "assistant", "content": "ack"},
    {"role": "user", "content": "do it"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "t2", "name": "edit", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t2", "content": "edited"}]}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 3)
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()

	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Get("role").String())
	assert.Equal(t, "assistant", msgs[1].Get("role").String())
	assert.Equal(t, "edit", msgs[1].Get("content.0.name").String())
	assert.Equal(t, "user", msgs[2].Get("role").String())
	assert.Equal(t, "t2", msgs[2].Get("content.0.tool_use_id").String())
}

func TestTrimLastN_StripsOrphanedOpenAIToolMessages(t *testing.T) {
	t.Parallel()

	const body = `{
  "model": "gpt-5",
  "messages": [
    {"role": "system", "content": "sys"},
    {"role": "user", "content": "hi"},
    {"role": "assistant", "content": null, "tool_calls": [{"id": "tc1", "type": "function", "function": {"name": "search", "arguments": "{}"}}]},
    {"role": "tool", "tool_call_id": "tc1", "content": "result"},
    {"role": "assistant", "content": "here you go"},
    {"role": "user", "content": "thanks"}
  ]
}`
	env, err := translate.ParseOpenAI([]byte(body))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 3)
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareOpenAI(nil, translate.EmitOptions{TargetModel: "gpt-5"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()

	// system preserved + orphaned tool message stripped → [system, assistant, user]
	require.Len(t, msgs, 3)
	assert.Equal(t, "system", msgs[0].Get("role").String())
	assert.Equal(t, "assistant", msgs[1].Get("role").String())
	assert.Equal(t, "here you go", msgs[1].Get("content").String())
	assert.Equal(t, "user", msgs[2].Get("role").String())
}

func TestRewriteEnvelope_StripsToolResultsFromLatestUser(t *testing.T) {
	t.Parallel()

	// Conversation ends with a user tool_result message (mid-tool-use).
	const body = `{
  "model": "claude-opus-4-7",
  "system": "sys",
  "messages": [
    {"role": "user", "content": "run a search"},
    {"role": "assistant", "content": [{"type": "tool_use", "id": "t1", "name": "search", "input": {}}]},
    {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "t1", "content": "found it"}]}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "User asked for a search.")

	// Latest user had only tool_results which are all orphaned after rewrite
	// (summary has no tool_use). It gets dropped → only summary remains.
	assert.Equal(t, 3, elided)

	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: "claude-opus-4-7"})
	require.NoError(t, err)
	msgs := gjson.GetBytes(prep.Body, "messages").Array()
	require.Len(t, msgs, 1, "only summary when latest user was purely tool_results")
	assert.Equal(t, "assistant", msgs[0].Get("role").String())
}

func TestRewriteEnvelope_GeminiCollapsesToSummaryPlusLastUser(t *testing.T) {
	t.Parallel()

	const geminiConversation = `{
  "contents": [
    {"role": "user", "parts": [{"text": "Plan a refactor of pkg/foo."}]},
    {"role": "model", "parts": [{"text": "Sure — I will start with renames."}]},
    {"role": "user", "parts": [{"text": "Now apply step 1."}]},
    {"role": "model", "parts": [{"functionCall": {"name": "edit", "args": {}}}]},
    {"role": "user", "parts": [{"functionResponse": {"name": "edit", "response": {"result": "edit applied"}}}]},
    {"role": "user", "parts": [{"text": "Continue with step 2."}]}
  ]
}`
	env, err := translate.ParseGemini([]byte(geminiConversation))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "Refactor of pkg/foo in progress; step 1 applied.")

	// 6 original entries, only the trailing user is preserved → 5 elided.
	assert.Equal(t, 5, elided)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro"})
	require.NoError(t, err)

	contents := gjson.GetBytes(prep.Body, "contents").Array()
	require.Len(t, contents, 2, "expect [summary, lastUser]")

	assert.Equal(t, "model", contents[0].Get("role").String())
	summaryText := contents[0].Get("parts.0.text").String()
	assert.True(t, strings.HasPrefix(summaryText, translate.HandoverSummaryTag), "summary must carry the tag prefix; got %q", summaryText)
	assert.Contains(t, summaryText, "Refactor of pkg/foo")

	assert.Equal(t, "user", contents[1].Get("role").String())
	assert.Equal(t, "Continue with step 2.", contents[1].Get("parts.0.text").String())
}

func TestRewriteEnvelope_GeminiNoUserMessagesYieldsSummaryOnlyContents(t *testing.T) {
	t.Parallel()

	const body = `{
  "contents": [
    {"role": "model", "parts": [{"text": "hello"}]},
    {"role": "model", "parts": [{"text": "still talking to myself"}]}
  ]
}`
	env, err := translate.ParseGemini([]byte(body))
	require.NoError(t, err)

	elided := handover.RewriteEnvelope(env, "Brief recap.")

	// Both original model turns dropped; no user turn to keep.
	assert.Equal(t, 2, elided)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro"})
	require.NoError(t, err)
	contents := gjson.GetBytes(prep.Body, "contents").Array()
	require.Len(t, contents, 1, "expect summary-only contents when no user turn is present")
	assert.Equal(t, "model", contents[0].Get("role").String())
}

func TestTrimLastN_GeminiKeepsLastNAndSystemInstruction(t *testing.T) {
	t.Parallel()

	const tenEntryBody = `{
  "systemInstruction": {"parts": [{"text": "sys"}]},
  "contents": [
    {"role": "user", "parts": [{"text": "m1"}]},
    {"role": "model", "parts": [{"text": "m2"}]},
    {"role": "user", "parts": [{"text": "m3"}]},
    {"role": "model", "parts": [{"text": "m4"}]},
    {"role": "user", "parts": [{"text": "m5"}]},
    {"role": "model", "parts": [{"text": "m6"}]},
    {"role": "user", "parts": [{"text": "m7"}]},
    {"role": "model", "parts": [{"text": "m8"}]},
    {"role": "user", "parts": [{"text": "m9"}]},
    {"role": "model", "parts": [{"text": "m10"}]}
  ]
}`
	env, err := translate.ParseGemini([]byte(tenEntryBody))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 3)
	assert.Equal(t, 7, elided)

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro"})
	require.NoError(t, err)
	assert.Equal(t, "sys", gjson.GetBytes(prep.Body, "systemInstruction.parts.0.text").String(),
		"systemInstruction is untouched by TrimLastN on the Gemini path")
	contents := gjson.GetBytes(prep.Body, "contents").Array()
	require.Len(t, contents, 3)
	assert.Equal(t, "m8", contents[0].Get("parts.0.text").String())
	assert.Equal(t, "m9", contents[1].Get("parts.0.text").String())
	assert.Equal(t, "m10", contents[2].Get("parts.0.text").String())
}

func TestTrimLastN_GeminiNoOpWhenUnderLimit(t *testing.T) {
	t.Parallel()

	const body = `{
  "contents": [
    {"role": "user", "parts": [{"text": "m1"}]},
    {"role": "model", "parts": [{"text": "m2"}]}
  ]
}`
	env, err := translate.ParseGemini([]byte(body))
	require.NoError(t, err)

	elided := handover.TrimLastN(env, 5)
	assert.Equal(t, 0, elided, "fewer entries than n must elide nothing")

	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro"})
	require.NoError(t, err)
	contents := gjson.GetBytes(prep.Body, "contents").Array()
	require.Len(t, contents, 2)
}

// Regression: mid-session model switch + TrimLastN can orphan tool_result
// blocks, which the Anthropic→Gemini translation turned into an empty
// functionResponse.name (Gemini 400).
func TestTrimLastN_ThenPrepareGemini_NoEmptyFunctionResponseName(t *testing.T) {
	t.Parallel()

	// TrimLastN(3) orphans the tool_result in msg[4].
	const body = `{
  "model": "claude-opus-4-7",
  "system": "You are a helpful assistant.",
  "messages": [
    {"role": "user", "content": "explain deepinfra pricing"},
    {"role": "assistant", "content": [
      {"type": "text", "text": "Let me look that up."},
      {"type": "tool_use", "id": "tu_web1", "name": "WebFetch", "input": {"url": "https://deepinfra.com/pricing"}},
      {"type": "tool_use", "id": "tu_web2", "name": "WebSearch", "input": {"query": "deepinfra pricing"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "tu_web1", "content": "pricing page content"},
      {"type": "tool_result", "tool_use_id": "tu_web2", "content": "search results"}
    ]},
    {"role": "assistant", "content": [{"type": "text", "text": "Here are the pricing details..."}]},
    {"role": "user", "content": "now compare with fireworks"},
    {"role": "assistant", "content": [
      {"type": "tool_use", "id": "tu_web3", "name": "WebFetch", "input": {"url": "https://fireworks.ai/pricing"}},
      {"type": "tool_use", "id": "tu_web4", "name": "WebSearch", "input": {"query": "fireworks pricing"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "tu_web3", "content": "fireworks pricing"},
      {"type": "tool_result", "tool_use_id": "tu_web4", "content": "fireworks search results"}
    ]},
    {"role": "assistant", "content": [{"type": "text", "text": "Fireworks costs $X..."}]},
    {"role": "user", "content": "give me similar analysis for together.ai"}
  ]
}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)

	// Simulate the handover switch path (summarizer not wired → TrimLastN).
	handover.TrimLastN(env, 3)

	// Now translate to Gemini — this is the path that produced the 400.
	prep, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{
		TargetModel:  "gemini-3.1-flash-lite-preview",
		Capabilities: router.ModelSpec{},
	})
	require.NoError(t, err)

	// Walk every part in the Gemini output and verify no functionResponse
	// has an empty name.
	contents := gjson.GetBytes(prep.Body, "contents")
	require.True(t, contents.IsArray(), "expected contents array")

	contents.ForEach(func(_, entry gjson.Result) bool {
		entry.Get("parts").ForEach(func(_, part gjson.Result) bool {
			fr := part.Get("functionResponse")
			if !fr.Exists() {
				return true
			}
			name := fr.Get("name").String()
			assert.NotEmpty(t, name,
				"functionResponse.name must not be empty (would cause Gemini 400); contents entry role=%s",
				entry.Get("role").String())
			return true
		})
		return true
	})

	// Also verify the conversation is structurally valid — should have the
	// trimmed messages without orphaned tool results.
	contentsArr := contents.Array()
	assert.GreaterOrEqual(t, len(contentsArr), 2, "should have at least the last assistant + user turns")
}
