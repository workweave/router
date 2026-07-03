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
