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

// openAIToolCallStopStream streams a tool_calls delta and then closes the turn
// with finish_reason "stop" — the shape some OpenAI-compat upstreams emit for a
// tool-emitting turn. The footer must stay off this turn.
func openAIToolCallStopStream() string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"ls","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

func TestOpenAIRoutingFooterWriter_SkipsToolCallsClosedWithStop(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(openAIToolCallStopStream()))
	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "Was this routing right?",
		"a turn that streamed tool_calls must not get a footer even when it ends with finish_reason stop")
}

// openAICoalescedStream packs the last answer token and finish_reason "stop"
// into a single chunk, as some OpenAI-compat upstreams do.
func openAICoalescedStream() string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

func TestOpenAIRoutingFooterWriter_CoalescedFooterAfterText(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(openAICoalescedStream()))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	textIdx, footerIdx, finishIdx := -1, -1, -1
	for i, e := range events {
		data := extractDataField(e)
		content := gjson.Get(data, "choices.0.delta.content").String()
		if content == "Hi" {
			textIdx = i
		}
		if strings.Contains(content, "Was this routing right?") {
			footerIdx = i
		}
		if gjson.Get(data, "choices.0.finish_reason").String() == "stop" {
			finishIdx = i
		}
	}
	require.NotEqual(t, -1, textIdx, "answer text chunk must be present")
	require.NotEqual(t, -1, footerIdx, "footer chunk must be present")
	require.NotEqual(t, -1, finishIdx, "finish chunk must be present")
	assert.Less(t, textIdx, footerIdx, "answer text must precede the footer")
	assert.Less(t, footerIdx, finishIdx, "footer must precede the finish chunk")
}

// openAIEmptyToolCallsStream emits an answer chunk that carries an empty
// "tool_calls":[] alongside content, then a natural finish. The empty array
// must not be treated as a tool step.
func openAIEmptyToolCallsStream() string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi","tool_calls":[]},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
}

func TestOpenAIRoutingFooterWriter_EmptyToolCallsStillInjects(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(openAIEmptyToolCallsStream()))
	require.NoError(t, err)
	assert.Contains(t, rec.Body.String(), "Was this routing right?",
		"an empty tool_calls array must not suppress the footer on an answer-only turn")
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
