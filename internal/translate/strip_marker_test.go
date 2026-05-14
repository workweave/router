package translate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const markerSentinel = "✦ **Weave Router**"

// Sample markers covering every humanReasonFromPlanner output and the
// no-planner fallbacks ("hard pin", "tool-result follow-up", "top scorer")
// plus the clamp note tail. Keep in sync with internal/proxy/service.go.
var sampleMarkers = []string{
	"✦ **Weave Router** → claude-haiku-4-5 (anthropic) · reason: top scorer\n\n",
	"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: scorer matches the pin\n\n",
	"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: tool-result follow-up\n\n",
	"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: switched to save on cache reads\n\n",
	"✦ **Weave Router** → deepseek/deepseek-v4-pro (openrouter) · reason: stayed: cache reuse beats the switch\n\n",
	"✦ **Weave Router** → claude-sonnet-4-5 (anthropic) · reason: model tier upgrade\n\n",
	"✦ **Weave Router** → gemini-3.1-flash-lite-preview (google) · reason: hard pin (compaction / sub-agent)\n\n",
	"✦ **Weave Router** → claude-opus-4-7 · reason: top scorer · second-choice pick (would have used claude-sonnet-4-5 — capped to your requested mid tier; request claude-opus-4-7 to unlock higher-tier picks)\n\n",
}

func TestStripRoutingMarker_AssistantBlockExactMatch(t *testing.T) {
	marker := sampleMarkers[1]
	body := buildBody(t, []map[string]any{
		{"role": "user", "content": []any{textBlock("hi")}},
		{"role": "assistant", "content": []any{textBlock(marker), textBlock("real response")}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), markerSentinel)

	content := gjson.GetBytes(out, "messages.1.content")
	require.True(t, content.IsArray())
	assert.Equal(t, 1, len(content.Array()), "marker-only block dropped, real text retained")
	assert.Equal(t, "real response", content.Get("0.text").String())
}

func TestStripRoutingMarker_OnlyMarkerBlockDropsToEmptyContent(t *testing.T) {
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock(sampleMarkers[0])}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), markerSentinel)

	content := gjson.GetBytes(out, "messages.0.content")
	require.True(t, content.IsArray())
	assert.Equal(t, 0, len(content.Array()), "sole marker block dropped, leaving empty content")
}

func TestStripRoutingMarker_EmbeddedInUserBlockStripsSubstringOnly(t *testing.T) {
	// Mirrors the sub-agent quoted-result case observed in production: marker
	// sits inside a wrapping <task-notification>/<result> envelope, the rest
	// of the text must survive verbatim.
	embedded := "</summary>\n<result>" + sampleMarkers[2] + "</result>\n<usage><total_tokens>0</total_tokens></usage>"
	body := buildBody(t, []map[string]any{
		{"role": "user", "content": []any{textBlock(embedded)}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), markerSentinel)

	text := gjson.GetBytes(out, "messages.0.content.0.text").String()
	assert.True(t, strings.HasPrefix(text, "</summary>\n<result>"))
	assert.True(t, strings.HasSuffix(text, "</usage>"))
}

func TestStripRoutingMarker_MultipleOccurrencesInOneBlock(t *testing.T) {
	text := sampleMarkers[0] + "some prose " + sampleMarkers[1] + "more prose " + sampleMarkers[2]
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock(text)}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), markerSentinel)
	stripped := gjson.GetBytes(out, "messages.0.content.0.text").String()
	assert.Equal(t, "some prose more prose ", stripped)
}

func TestStripRoutingMarker_NoMarkerReturnsIdenticalSlice(t *testing.T) {
	body := buildBody(t, []map[string]any{
		{"role": "user", "content": []any{textBlock("plain prompt")}},
		{"role": "assistant", "content": []any{textBlock("plain response")}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	require.Equal(t, len(body), len(out))
	// Identity check: when nothing changes we hand back the exact input slice.
	assert.True(t, &body[0] == &out[0], "expected the same backing array when no marker present")
}

func TestStripRoutingMarker_AllPlannerReasonShapes(t *testing.T) {
	for _, marker := range sampleMarkers {
		t.Run(marker[:min(60, len(marker))], func(t *testing.T) {
			body := buildBody(t, []map[string]any{
				{"role": "assistant", "content": []any{textBlock(marker + "trailing model text")}},
			})
			out, err := translate.StripRoutingMarkerFromMessages(body)
			require.NoError(t, err)
			assert.NotContains(t, string(out), markerSentinel)
			assert.Equal(t, "trailing model text", gjson.GetBytes(out, "messages.0.content.0.text").String())
		})
	}
}

func TestStripRoutingMarker_NonTextBlocksUntouched(t *testing.T) {
	toolUse := map[string]any{
		"type":  "tool_use",
		"id":    "toolu_abc",
		"name":  "Bash",
		"input": map[string]any{"command": "ls"},
	}
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock(sampleMarkers[1]), toolUse}},
	})

	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	content := gjson.GetBytes(out, "messages.0.content")
	require.Equal(t, 1, len(content.Array()))
	assert.Equal(t, "tool_use", content.Get("0.type").String())
	assert.Equal(t, "Bash", content.Get("0.name").String())
}

func TestStripRoutingMarker_EmptyAndMissingMessages(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"x","messages":[]}`),
		[]byte(`{"model":"x"}`),
		[]byte(`{"model":"x","messages":[{"role":"user","content":"plain string"}]}`),
	}
	for _, body := range cases {
		out, err := translate.StripRoutingMarkerFromMessages(body)
		require.NoError(t, err)
		assert.Equal(t, body, out)
	}
}

func TestStripRoutingMarker_StringContentNoOp(t *testing.T) {
	// Some clients send messages[].content as a plain string. The marker is
	// only injected inside content arrays, so string-content messages must be
	// returned unchanged.
	body := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	out, err := translate.StripRoutingMarkerFromMessages(body)
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

func TestStripRoutingMarker_PreservesAdjacentFields(t *testing.T) {
	marker := sampleMarkers[1]
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock(marker + "kept")}},
	})
	withExtras, err := json.Marshal(map[string]any{
		"model":       "claude-opus-4-7",
		"max_tokens":  64000,
		"system":      "you are helpful",
		"temperature": 0.7,
		"messages":    json.RawMessage(gjson.GetBytes(body, "messages").Raw),
	})
	require.NoError(t, err)

	out, err := translate.StripRoutingMarkerFromMessages(withExtras)
	require.NoError(t, err)
	assert.Equal(t, "claude-opus-4-7", gjson.GetBytes(out, "model").String())
	assert.Equal(t, int64(64000), gjson.GetBytes(out, "max_tokens").Int())
	assert.Equal(t, "you are helpful", gjson.GetBytes(out, "system").String())
	assert.InDelta(t, 0.7, gjson.GetBytes(out, "temperature").Float(), 1e-9)
	assert.Equal(t, "kept", gjson.GetBytes(out, "messages.0.content.0.text").String())
}

func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func buildBody(t *testing.T, messages []map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "claude-opus-4-7",
		"messages": messages,
	})
	require.NoError(t, err)
	return body
}

