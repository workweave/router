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

func TestGeminiRoutingMarkerWriter_StreamingInjectsMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingMarkerWriter(rec, "✦ **Weave Router** → gemini-2.0-flash · best pick for this turn\n\n")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	upstreamChunk := `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"},"index":0}]}` + "\n\n"
	_, err := w.Write([]byte(upstreamChunk))
	require.NoError(t, err)

	body := rec.Body.String()
	events := splitSSEEvents(body)
	require.Len(t, events, 2, "expected marker chunk + upstream chunk")

	markerData := strings.TrimPrefix(events[0], "data: ")
	text := gjson.Get(markerData, "candidates.0.content.parts.0.text").String()
	assert.Contains(t, text, "Weave Router")
	assert.Contains(t, text, "gemini-2.0-flash")
	assert.Equal(t, "model", gjson.Get(markerData, "candidates.0.content.role").String())
}

func TestGeminiRoutingMarkerWriter_EmptyMarkerNoInjection(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingMarkerWriter(rec, "")

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	upstreamChunk := `data: {"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}]}` + "\n\n"
	_, err := w.Write([]byte(upstreamChunk))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	assert.Len(t, events, 1, "empty marker should not inject a chunk")
}

func TestGeminiRoutingMarkerWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingMarkerWriter(rec, "✦ marker")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"candidates":[{"content":{"parts":[{"text":"Hello"}],"role":"model"}}]}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)

	assert.Equal(t, body, rec.Body.String(), "non-streaming responses should pass through unmodified")
}
