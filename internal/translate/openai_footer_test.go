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

// openAIAnswerStream is a one-token answer chunk + finish chunk + [DONE].
func openAIAnswerStream(finishReason string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"` + finishReason + `"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

func TestOpenAIRoutingFooterWriter_InjectsBeforeFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(openAIAnswerStream("stop")))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	// content chunk + footer chunk + finish chunk + [DONE] = 4.
	require.Len(t, events, 4)

	footerIdx, finishIdx := -1, -1
	for i, e := range events {
		data := extractDataField(e)
		if strings.Contains(gjson.Get(data, "choices.0.delta.content").String(), "Was this routing right?") {
			footerIdx = i
		}
		if gjson.Get(data, "choices.0.finish_reason").String() == "stop" {
			finishIdx = i
		}
	}
	require.NotEqual(t, -1, footerIdx, "footer chunk must be present")
	require.NotEqual(t, -1, finishIdx)
	assert.Less(t, footerIdx, finishIdx, "footer must precede the finish chunk")
}

func TestOpenAIRoutingFooterWriter_SkipsToolCalls(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(openAIAnswerStream("tool_calls")))
	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "Was this routing right?", "tool_calls turns must not get a footer")
}

func TestOpenAIRoutingFooterWriter_EmptyFooterPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, "")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	in := openAIAnswerStream("stop")
	_, err := w.Write([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, in, rec.Body.String())
}

func TestOpenAIRoutingFooterWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"id":"c1","choices":[{"message":{"content":"Hi"},"finish_reason":"stop"}]}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, body, rec.Body.String())
}
