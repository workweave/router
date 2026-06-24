package translate_test

import (
	"encoding/json"
	"testing"

	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const footerSentinel = "_Was this routing right?_"

// sampleFooter mirrors proxy.Service.feedbackFooter's clickable output shape: a
// leading blank-line separator, the prompt, two markdown thumb links to the
// signed rate endpoint, and the /rf keyboard companion. The token uses URL-safe
// base64 chars (-, _) to exercise the "[^)]*" URL match.
func sampleFooter() string {
	base := "https://feedback.example/v1/feedback/rate?t=abc-123_XYZ.sig&r="
	return "\n\n_Was this routing right?_ [👍](" + base + "up) [👎](" + base + "down) — or reply `/rf+` / `/rf-`"
}

func TestStripFeedbackFooter_AppendedToAssistantBlock(t *testing.T) {
	body := buildBody(t, []map[string]any{
		{"role": "user", "content": []any{textBlock("hi")}},
		{"role": "assistant", "content": []any{textBlock("real response" + sampleFooter())}},
	})

	out, err := translate.StripFeedbackFooterFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), footerSentinel)
	assert.Equal(t, "real response", gjson.GetBytes(out, "messages.1.content.0.text").String())
}

func TestStripFeedbackFooter_SoleFooterBlockDropped(t *testing.T) {
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock("real response"), textBlock(sampleFooter())}},
	})

	out, err := translate.StripFeedbackFooterFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), footerSentinel)

	content := gjson.GetBytes(out, "messages.0.content")
	require.True(t, content.IsArray())
	assert.Equal(t, 1, len(content.Array()), "footer-only block dropped, real text retained")
	assert.Equal(t, "real response", content.Get("0.text").String())
}

func TestStripFeedbackFooter_StringContent(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "assistant", "content": "Hello, how can I help?" + sampleFooter()},
		},
	})
	require.NoError(t, err)

	out, err := translate.StripFeedbackFooterFromMessages(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), footerSentinel)
	assert.Equal(t, "Hello, how can I help?", gjson.GetBytes(out, "messages.0.content").String())
}

func TestStripFeedbackFooter_NoFooterReturnsIdenticalSlice(t *testing.T) {
	body := buildBody(t, []map[string]any{
		{"role": "user", "content": []any{textBlock("plain prompt")}},
		{"role": "assistant", "content": []any{textBlock("plain response")}},
	})

	out, err := translate.StripFeedbackFooterFromMessages(body)
	require.NoError(t, err)
	require.Equal(t, len(body), len(out))
	assert.True(t, &body[0] == &out[0], "expected the same backing array when no footer present")
}

func TestStripFeedbackFooter_NonTextBlocksUntouched(t *testing.T) {
	toolUse := map[string]any{
		"type":  "tool_use",
		"id":    "toolu_abc",
		"name":  "Bash",
		"input": map[string]any{"command": "ls"},
	}
	body := buildBody(t, []map[string]any{
		{"role": "assistant", "content": []any{textBlock("answer" + sampleFooter()), toolUse}},
	})

	out, err := translate.StripFeedbackFooterFromMessages(body)
	require.NoError(t, err)
	content := gjson.GetBytes(out, "messages.0.content")
	require.Equal(t, 2, len(content.Array()))
	assert.Equal(t, "answer", content.Get("0.text").String())
	assert.Equal(t, "tool_use", content.Get("1.type").String())
}

func TestStripFeedbackFooter_GeminiContentsDropsSoleFooterPart(t *testing.T) {
	body := geminiBody(t, []map[string]any{
		{"role": "user", "parts": []any{geminiTextPart("hi")}},
		{"role": "model", "parts": []any{geminiTextPart("real response"), geminiTextPart(sampleFooter())}},
	})

	out, err := translate.StripFeedbackFooterFromGeminiContents(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), footerSentinel)

	parts := gjson.GetBytes(out, "contents.1.parts")
	require.True(t, parts.IsArray())
	assert.Equal(t, 1, len(parts.Array()), "footer-only part dropped, real text retained")
	assert.Equal(t, "real response", parts.Get("0.text").String())
}

func TestStripFeedbackFooter_GeminiContentsStripsAppendedPart(t *testing.T) {
	body := geminiBody(t, []map[string]any{
		{"role": "model", "parts": []any{geminiTextPart("real response" + sampleFooter())}},
	})

	out, err := translate.StripFeedbackFooterFromGeminiContents(body)
	require.NoError(t, err)
	assert.NotContains(t, string(out), footerSentinel)
	assert.Equal(t, "real response", gjson.GetBytes(out, "contents.0.parts.0.text").String())
}

func TestStripFeedbackFooter_GeminiNoFooterReturnsIdenticalSlice(t *testing.T) {
	body := geminiBody(t, []map[string]any{
		{"role": "model", "parts": []any{geminiTextPart("plain response")}},
	})

	out, err := translate.StripFeedbackFooterFromGeminiContents(body)
	require.NoError(t, err)
	require.Equal(t, len(body), len(out))
	assert.True(t, &body[0] == &out[0], "expected the same backing array when no footer present")
}

func TestStripFeedbackFooter_EmptyAndMissing(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"model":"x","messages":[]}`),
		[]byte(`{"model":"x"}`),
		[]byte(`{"model":"x","contents":[]}`),
	}
	for _, body := range cases {
		out, err := translate.StripFeedbackFooterFromMessages(body)
		require.NoError(t, err)
		assert.Equal(t, body, out)

		out, err = translate.StripFeedbackFooterFromGeminiContents(body)
		require.NoError(t, err)
		assert.Equal(t, body, out)
	}
}

func geminiTextPart(text string) map[string]any {
	return map[string]any{"text": text}
}

func geminiBody(t *testing.T, contents []map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "gemini-3.1-pro-preview",
		"contents": contents,
	})
	require.NoError(t, err)
	return body
}
