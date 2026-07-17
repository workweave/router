package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGeminiToOpenAISSETranslator_IncompleteEOFEmitsFailureNotStop(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewGeminiToOpenAISSETranslator(rec, "gemini-x", nil)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}\n\n"))
	require.NoError(t, err)
	require.ErrorIs(t, w.Finalize(), translate.ErrStreamIncomplete)
	assert.Contains(t, rec.Body.String(), `"type":"api_error"`)
	assert.NotContains(t, rec.Body.String(), "finish_reason\":\"stop")
	assert.NotContains(t, rec.Body.String(), "[DONE]")
}

func TestSSETranslator_IncompleteEOFEmitsFailureNotDone(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewSSETranslator(rec, "gpt-x", nil)
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte("event: message_start\ndata: {\"message\":{\"id\":\"m\",\"model\":\"gpt-x\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
	require.NoError(t, err)
	require.ErrorIs(t, w.Finalize(), translate.ErrStreamIncomplete)
	assert.Contains(t, rec.Body.String(), `"type":"api_error"`)
	assert.NotContains(t, rec.Body.String(), "[DONE]")
}

func TestResponsesWriter_IncompleteEOFTerminatesAsFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "gpt-x")
	require.NoError(t, w.Prelude(true))
	_, err := w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))
	require.NoError(t, err)
	require.ErrorIs(t, w.Finalize(), translate.ErrStreamIncomplete)
	assert.Contains(t, rec.Body.String(), "response.failed")
	assert.NotContains(t, rec.Body.String(), "response.completed")
}
