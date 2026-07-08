package translate_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/tidwall/gjson"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: Kimi-k2.x (and other OpenAI-compat upstreams) emit tool_call
// ids like "functions.Read:0" containing dots/colons, which fail Anthropic's
// required tool_use.id pattern (^[a-zA-Z0-9_-]+$). The non-streaming emit
// path already sanitized these; the streaming SSE tool_use emitter did not,
// letting the raw id reach the client and later get replayed straight to
// Anthropic on `resume`, producing a 400.
func TestAnthropicSSETranslator_SanitizesStreamedToolUseID(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "moonshotai/kimi-k2.7", nil)
	require.NoError(t, w.Prelude(true))

	feedChat(t, w,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"functions.Read:0","function":{"name":"Read","arguments":""}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	)

	out := rec.Body.String()
	var sawToolUse bool
	for _, line := range strings.Split(out, "\n") {
		const p = "data: "
		if !strings.HasPrefix(line, p) {
			continue
		}
		data := line[len(p):]
		block := gjson.Get(data, "content_block")
		if block.Get("type").String() != "tool_use" {
			continue
		}
		sawToolUse = true
		id := block.Get("id").String()
		assert.Regexp(t, `^[a-zA-Z0-9_-]+$`, id, "streamed tool_use.id must match Anthropic's required pattern")
		assert.NotContains(t, id, ".")
		assert.NotContains(t, id, ":")
	}
	assert.True(t, sawToolUse, "expected a tool_use content_block_start event")
}

// streamedToolUseIDs returns every tool_use.id from an Anthropic SSE body.
func streamedToolUseIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		const p = "data: "
		if !strings.HasPrefix(line, p) {
			continue
		}
		block := gjson.Get(line[len(p):], "content_block")
		if block.Get("type").String() == "tool_use" {
			ids = append(ids, block.Get("id").String())
		}
	}
	return ids
}

// Regression: deterministic upstreams (Kimi-k2.x on Fireworks) emit the same
// tool_call id every turn; after sanitization the id repeated. Claude Code
// dedupes by id and drops repeats, stalling the session. Each translated
// response must yield a distinct tool_use.id even for byte-identical upstream ids.
func TestAnthropicSSETranslator_StreamedToolUseIDUniqueAcrossResponses(t *testing.T) {
	feedOnce := func() string {
		rec := httptest.NewRecorder()
		w := translate.NewAnthropicSSETranslator(rec, "moonshotai/kimi-k2.7", nil)
		require.NoError(t, w.Prelude(true))
		feedChat(t, w,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"functions.Bash:0","function":{"name":"Bash","arguments":""}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		)
		ids := streamedToolUseIDs(rec.Body.String())
		require.Len(t, ids, 1)
		return ids[0]
	}

	first := feedOnce()
	second := feedOnce()

	// Both must be sanitized (no dots/colons) and both must derive from the
	// upstream id, but the per-response nonce must make them differ.
	assert.Regexp(t, `^functions_Bash_0_[0-9a-f]{12}$`, first)
	assert.Regexp(t, `^functions_Bash_0_[0-9a-f]{12}$`, second)
	assert.NotEqual(t, first, second, "identical upstream ids must translate to distinct tool_use ids across responses")
}

// Within a single response, all blocks share one nonce so a tool_use.id and
// its later tool_result.tool_use_id (which the client echoes back) still pair,
// and distinct upstream ids stay distinct.
func TestAnthropicSSETranslator_StreamedToolUseIDStableWithinResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicSSETranslator(rec, "moonshotai/kimi-k2.7", nil)
	require.NoError(t, w.Prelude(true))
	feedChat(t, w,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"functions.Bash:0","function":{"name":"Bash","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"functions.Read:1","function":{"name":"Read","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	ids := streamedToolUseIDs(rec.Body.String())
	require.Len(t, ids, 2)
	assert.Regexp(t, `^functions_Bash_0_[0-9a-f]{12}$`, ids[0])
	assert.Regexp(t, `^functions_Read_1_[0-9a-f]{12}$`, ids[1])
	// Same response → same nonce suffix on both.
	suffix := func(id string) string { return id[strings.LastIndex(id, "_")+1:] }
	assert.Equal(t, suffix(ids[0]), suffix(ids[1]), "all tool_use ids in one response share the response nonce")
	assert.NotEqual(t, ids[0], ids[1], "distinct upstream ids stay distinct")
}
