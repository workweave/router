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

func TestOpenAIRoutingMarkerWriter_StreamingInjectsMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "✦ **Weave Router** → gpt-4o · best pick for this turn\n\n")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	upstreamChunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
	_, err := w.Write([]byte(upstreamChunk))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)
	require.Len(t, events, 2, "expected marker chunk + upstream chunk")

	marker := events[0]
	assert.Contains(t, marker, `"chat.completion.chunk"`)
	markerData := strings.TrimPrefix(marker, "data: ")
	assert.Equal(t, "assistant", gjson.Get(markerData, "choices.0.delta.role").String())
	assert.Contains(t, gjson.Get(markerData, "choices.0.delta.content").String(), "Weave Router")
	assert.Contains(t, gjson.Get(markerData, "choices.0.delta.content").String(), "gpt-4o")
	assert.Equal(t, "gpt-4o", gjson.Get(markerData, "model").String())
}

func TestOpenAIRoutingMarkerWriter_EmptyMarkerNoInjection(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	upstreamChunk := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n"
	_, err := w.Write([]byte(upstreamChunk))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	assert.Len(t, events, 1, "empty marker should not inject a chunk")
}

func TestOpenAIRoutingMarkerWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "✦ marker")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"Hello"}}]}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)

	assert.Equal(t, body, rec.Body.String(), "non-streaming responses should pass through unmodified")
}

func TestOpenAIRoutingMarkerWriter_ErrorResponsePassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "✦ marker")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusTooManyRequests)

	errBody := `{"error":{"message":"rate limited"}}`
	_, err := w.Write([]byte(errBody))
	require.NoError(t, err)

	assert.Equal(t, errBody, rec.Body.String(), "error responses should pass through unmodified")
}

func TestOpenAIRoutingMarkerWriter_MarkerEmittedOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "✦ marker\n\n")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	chunk1 := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	chunk2 := "data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n"
	_, _ = w.Write([]byte(chunk1))
	_, _ = w.Write([]byte(chunk2))

	events := splitSSEEvents(rec.Body.String())
	assert.Len(t, events, 3, "marker + 2 upstream chunks")

	markerCount := 0
	for _, e := range events {
		if strings.Contains(e, "Weave") {
			// Not all markers contain "Weave" — count by the specific marker content.
		}
		data := strings.TrimPrefix(e, "data: ")
		if content := gjson.Get(data, "choices.0.delta.content").String(); strings.Contains(content, "marker") {
			markerCount++
		}
	}
	assert.Equal(t, 1, markerCount, "marker should be emitted exactly once")
}

func TestOpenAIRoutingMarkerWriter_PreludeFiresBeforeUpstream(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "✦ Weave Router → gpt-4o\n\n")

	require.NoError(t, w.Prelude(true))

	// Prelude must have flushed both HTTP 200 + the marker chunk before any upstream byte.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	events := splitSSEEvents(rec.Body.String())
	require.Len(t, events, 1, "prelude should emit exactly one marker chunk")
	assert.Contains(t, events[0], "Weave Router")

	// A subsequent upstream chunk must NOT trigger a duplicate marker.
	upstream := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"
	_, err := w.Write([]byte(upstream))
	require.NoError(t, err)
	events = splitSSEEvents(rec.Body.String())
	require.Len(t, events, 2, "marker should be emitted exactly once across prelude + upstream")
}

func TestOpenAIRoutingMarkerWriter_PreludeNoOpWhenNonStreaming(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewOpenAIRoutingMarkerWriter(rec, "gpt-4o", "marker")

	require.NoError(t, w.Prelude(false))
	assert.Empty(t, rec.Body.String(), "non-streaming Prelude must write nothing")
}

// splitSSEEvents splits SSE text into individual events (each terminated by \n\n).
func splitSSEEvents(s string) []string {
	raw := strings.Split(s, "\n\n")
	var events []string
	for _, e := range raw {
		e = strings.TrimSpace(e)
		if e != "" {
			events = append(events, e)
		}
	}
	return events
}
