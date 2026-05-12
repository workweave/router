//go:build google_integration
// +build google_integration

package google_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/google"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNative_TwoTurnToolUseRoundTripsThoughtSignature asserts a complete
// tool-use loop succeeds against the real Gemini native API: the model emits
// a functionCall with a thoughtSignature; we round-trip that signature with a
// functionResponse; turn 2 must not 400 on a missing signature. Run with
// `GOOGLE_API_KEY=... go test -tags=google_integration`.
func TestNative_TwoTurnToolUseRoundTripsThoughtSignature(t *testing.T) {
	key := os.Getenv("GOOGLE_API_KEY")
	if key == "" {
		t.Skip("GOOGLE_API_KEY not set")
	}

	const model = "gemini-2.5-flash"
	c := google.NewNativeClient(key, "")

	turn1 := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "What is the weather in San Francisco? Use the get_weather tool."}}},
		},
		"tools": []any{
			map[string]any{"functionDeclarations": []any{
				map[string]any{
					"name":        "get_weather",
					"description": "Get current weather for a location",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{"type": "string"},
						},
						"required": []any{"location"},
					},
				},
			}},
		},
		"toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}},
	}
	body1, err := json.Marshal(turn1)
	require.NoError(t, err)

	rec1 := httptest.NewRecorder()
	prep1 := providers.PreparedRequest{Body: body1, Headers: make(http.Header)}
	require.NoError(t, c.Proxy(context.Background(),
		router.Decision{Model: model}, prep1, rec1,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(""))))

	require.Equal(t, http.StatusOK, rec1.Code, "turn 1 must succeed; got body: %s", rec1.Body.String())

	var resp1 map[string]any
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	candidates, _ := resp1["candidates"].([]any)
	require.NotEmpty(t, candidates, "turn 1 must return a candidate; got: %s", rec1.Body.String())
	parts := candidates[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)

	var fcPart map[string]any
	for _, p := range parts {
		if pm, ok := p.(map[string]any); ok {
			if _, hasFC := pm["functionCall"]; hasFC {
				fcPart = pm
				break
			}
		}
	}
	require.NotNil(t, fcPart, "turn 1 must produce a functionCall part; parts=%v", parts)

	sig, _ := fcPart["thoughtSignature"].(string)
	t.Logf("turn 1 thoughtSignature length: %d", len(sig))
	// Gemini 3.x requires the signature; Gemini 2.5 may omit it. Round-trip succeeds either way.

	assistantPart := map[string]any{
		"functionCall": fcPart["functionCall"],
	}
	if sig != "" {
		assistantPart["thoughtSignature"] = sig
	}
	turn2 := map[string]any{
		"contents": []any{
			turn1["contents"].([]any)[0],
			map[string]any{"role": "model", "parts": []any{assistantPart}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{
					"name":     fcPart["functionCall"].(map[string]any)["name"],
					"response": map[string]any{"result": "62°F and sunny"},
				}},
			}},
		},
		"tools":      turn1["tools"],
		"toolConfig": map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}},
	}
	body2, err := json.Marshal(turn2)
	require.NoError(t, err)

	rec2 := httptest.NewRecorder()
	prep2 := providers.PreparedRequest{Body: body2, Headers: make(http.Header)}
	require.NoError(t, c.Proxy(context.Background(),
		router.Decision{Model: model}, prep2, rec2,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader(""))))

	require.Equal(t, http.StatusOK, rec2.Code,
		"turn 2 must succeed (no thought_signature 400); body: %s", rec2.Body.String())

	var resp2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	t.Logf("turn 2 response: %s", rec2.Body.String())

	cands2 := resp2["candidates"].([]any)
	require.NotEmpty(t, cands2)
}

func TestNative_GenerateContentTextOnly(t *testing.T) {
	key := os.Getenv("GOOGLE_API_KEY")
	if key == "" {
		t.Skip("GOOGLE_API_KEY not set")
	}
	c := google.NewNativeClient(key, "")
	body, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Reply with a single word: hello"}}},
		},
	})
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(),
		router.Decision{Model: "gemini-2.5-flash"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("")))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// TestNative_StreamGenerateContent verifies the streaming hint flips the URL to :streamGenerateContent?alt=sse.
func TestNative_StreamGenerateContent(t *testing.T) {
	key := os.Getenv("GOOGLE_API_KEY")
	if key == "" {
		t.Skip("GOOGLE_API_KEY not set")
	}
	c := google.NewNativeClient(key, "")
	body, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "Count to three."}}},
		},
	})
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	prep.Headers.Set(translate.GeminiStreamHintHeader, "true")
	err := c.Proxy(context.Background(),
		router.Decision{Model: "gemini-2.5-flash"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("")))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream",
		"streaming response must advertise SSE content-type")
	assert.Contains(t, rec.Body.String(), "data:")
}
