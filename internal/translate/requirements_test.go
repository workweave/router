package translate

import (
	"testing"

	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslationRequirements_DetectsAnthropicPreservationSemantics(t *testing.T) {
	env, err := ParseAnthropic([]byte(`{
        "model":"claude-sonnet-4-5",
        "thinking":{"type":"enabled"},
        "output_config":{"format":{"type":"json_schema"}},
        "messages":[{"role":"user","content":[
          {"type":"document","source":{"type":"text","media_type":"text/plain","data":"notes"}},
          {"type":"audio","source":{"type":"base64","media_type":"audio/wav","data":"AA=="}}
        ]}],
        "tools":[{"type":"web_search_20250305","name":"web_search"}],
        "system":[{"type":"text","text":"remember","cache_control":{"type":"ephemeral"}}]
    }`))
	require.NoError(t, err)

	req := env.TranslationRequirements(router.EndpointAnthropicMessages)
	assert.True(t, req.ReasoningReplay)
	assert.True(t, req.PromptCacheControl)
	assert.True(t, req.StructuredOutput)
	assert.True(t, req.Audio)
	assert.True(t, req.Files)
	assert.True(t, req.CitationsOrSearch)
}

func TestTranslationRequirements_DetectsOpenAIMediaAndSearch(t *testing.T) {
	env, err := ParseOpenAI([]byte(`{
        "model":"gpt-5.5",
        "messages":[{"role":"user","content":[
          {"type":"input_audio","input_audio":{"data":"AA==","format":"wav"}},
          {"type":"input_file","file_id":"file_123"}
        ]}],
        "tools":[{"type":"web_search_preview"}],
        "response_format":{"type":"json_object"}
    }`))
	require.NoError(t, err)

	req := env.TranslationRequirements(router.EndpointOpenAIChat)
	assert.True(t, req.Audio)
	assert.True(t, req.Files)
	assert.True(t, req.CitationsOrSearch)
	assert.True(t, req.StructuredOutput)
}

func TestTranslationRequirements_DetectsGeminiAudioAndFiles(t *testing.T) {
	env, err := ParseGemini([]byte(`{
        "contents":[{"role":"user","parts":[
          {"inlineData":{"mimeType":"audio/wav","data":"AA=="}},
          {"fileData":{"mimeType":"application/pdf","fileUri":"gs://bucket/doc.pdf"}}
        ]}]
    }`))
	require.NoError(t, err)

	req := env.TranslationRequirements(router.EndpointGeminiGenerate)
	assert.True(t, req.Audio)
	assert.True(t, req.Files)
}
