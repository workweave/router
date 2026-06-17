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

func geminiAnswerStream() string {
	return `data: {"candidates":[{"content":{"parts":[{"text":"The answer is 42."}],"role":"model"},"index":0}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP","index":0}]}` + "\n\n"
}

func geminiToolCallStream() string {
	return `data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"bash","args":{}}}],"role":"model"},"index":0}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP","index":0}]}` + "\n\n"
}

func TestGeminiRoutingFooterWriter_InjectsBeforeFinish(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(geminiAnswerStream()))
	require.NoError(t, err)

	events := splitSSEEvents(rec.Body.String())
	// text chunk + footer chunk + finish chunk = 3.
	require.Len(t, events, 3)

	footerIdx, finishIdx := -1, -1
	for i, e := range events {
		data := extractDataField(e)
		if strings.Contains(gjson.Get(data, "candidates.0.content.parts.0.text").String(), "Was this routing right?") {
			footerIdx = i
		}
		if gjson.Get(data, "candidates.0.finishReason").String() == "STOP" {
			finishIdx = i
		}
	}
	require.NotEqual(t, -1, footerIdx, "footer chunk must be present")
	require.NotEqual(t, -1, finishIdx)
	assert.Less(t, footerIdx, finishIdx, "footer must precede the finish chunk")
}

func TestGeminiRoutingFooterWriter_SkipsToolCallTurn(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	_, err := w.Write([]byte(geminiToolCallStream()))
	require.NoError(t, err)
	assert.NotContains(t, rec.Body.String(), "Was this routing right?", "functionCall turns must not get a footer")
}

func TestGeminiRoutingFooterWriter_EmptyFooterPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingFooterWriter(rec, "")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	in := geminiAnswerStream()
	_, err := w.Write([]byte(in))
	require.NoError(t, err)
	assert.Equal(t, in, rec.Body.String())
}

func TestGeminiRoutingFooterWriter_NonStreamingPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiRoutingFooterWriter(rec, testFooter)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	body := `{"candidates":[{"content":{"parts":[{"text":"Hi"}]},"finishReason":"STOP"}]}`
	_, err := w.Write([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, body, rec.Body.String())
}
