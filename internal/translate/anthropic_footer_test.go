package translate_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const testFooter = "\n\n_Was this routing right?_ [👍](u) · [👎](d)"

// anthropicAnswerStream is a minimal text answer (index 0) that ends naturally.
func anthropicAnswerStream(stopReason string) string {
	return buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-4","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":0}}}`) +
		buildAnthropicSSE("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		buildAnthropicSSE("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"The answer is 42."}}`) +
		buildAnthropicSSE("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		buildAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stopReason+`","stop_sequence":null},"usage":{"output_tokens":10}}`) +
		buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)
}

func TestAnthropicRoutingFooterWriter_InjectsBeforeMessageDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(anthropicAnswerStream("end_turn")))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	// 6 upstream events + 3 footer block events (start/delta/stop) = 9.
	require.Len(t, events, 9)

	// Locate the footer text_delta and the message_delta; footer must precede it.
	footerIdx, msgDeltaIdx := -1, -1
	for i, e := range events {
		data := extractDataField(e)
		typ := gjson.Get(data, "type").String()
		if typ == "content_block_delta" && strings.Contains(gjson.Get(data, "delta.text").String(), "Was this routing right?") {
			footerIdx = i
			assert.EqualValues(t, 1, gjson.Get(data, "index").Int(), "footer block index should be maxIndex+1")
		}
		if typ == "message_delta" {
			msgDeltaIdx = i
		}
	}
	require.NotEqual(t, -1, footerIdx, "footer text_delta must be present")
	require.NotEqual(t, -1, msgDeltaIdx, "message_delta must be present")
	assert.Less(t, footerIdx, msgDeltaIdx, "footer must be injected before message_delta")
}

func TestAnthropicRoutingFooterWriter_SkipsToolUseTurn(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(anthropicAnswerStream("tool_use")))
	require.NoError(t, err)

	assert.NotContains(t, rec.Body.String(), "Was this routing right?", "tool_use turns must not get a footer")
}

func TestAnthropicRoutingFooterWriter_EmptyFooterPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, "")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	in := anthropicAnswerStream("end_turn")
	_, err := w.Write([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, in, rec.Body.String(), "empty footer must be byte-identical passthrough")
}

func TestAnthropicRoutingFooterWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"id":"msg_1","content":[{"type":"text","text":"Hi"}]}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, body, rec.Body.String(), "non-streaming must pass through unmodified")
}

func TestAnthropicRoutingFooterWriter_NoContentNoFooter(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	// message_start then message_delta(end_turn) with no content block at all.
	in := buildAnthropicSSE("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","content":[],"model":"x","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`) +
		buildAnthropicSSE("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`) +
		buildAnthropicSSE("message_stop", `{"type":"message_stop"}`)
	_, err := w.Write([]byte(in))
	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "Was this routing right?", "no content block means nothing to rate")
}

func TestAnthropicRoutingFooterWriter_EventSplitAcrossWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewAnthropicRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	full := anthropicAnswerStream("end_turn")
	mid := len(full) / 2
	_, err := w.Write([]byte(full[:mid]))
	require.NoError(t, err)
	_, err = w.Write([]byte(full[mid:]))
	require.NoError(t, err)

	body := rec.Body.String()
	assert.Contains(t, body, "Was this routing right?", "split stream must still get a footer")
	// Footer must appear exactly once.
	assert.Equal(t, 1, strings.Count(body, "Was this routing right?"))
}
