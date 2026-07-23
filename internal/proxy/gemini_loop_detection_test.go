package proxy_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/router/sessionpin"
	"workweave/router/internal/translate"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// geminiLoopEscalateModel mirrors proxy.geminiEscalateModel (unexported).
const geminiLoopEscalateModel = "gemini-3.1-pro-preview"

func newGeminiLoopSvc(fr *fakeRouter, store *fakePinStore, googleProv *fakeProvider) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{providers.ProviderGoogle: googleProv},
		nil,
		false,
		nil,
		store,
		false,
		providers.ProviderGoogle,
		"gemini-2.5-flash",
		nil,
	)
}

// buildGeminiTightLoopBody builds 5 identical bash {command: ls /tmp}
// functionCall/functionResponse pairs — enough to trip detectToolCallLoop
// (loopDetectionMaxRepeats == 5) without tripping the cyclic detector
// (needs ≥24 calls). Ends on a functionResponse turn so no genuine user
// text resets the window.
func buildGeminiTightLoopBody(t *testing.T) []byte {
	t.Helper()
	contents := []any{
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": "LOOP_DETECT_TIGHT"},
		}},
	}
	for i := 0; i < 5; i++ {
		contents = append(contents,
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{
					"name": "bash",
					"args": map[string]any{"command": "ls /tmp"},
				}},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{
					"name": "bash", "response": map[string]any{"result": "file1 file2"},
				}},
			}},
		)
	}
	body, err := json.Marshal(map[string]any{
		"model":    "gemini-2.5-flash",
		"stream":   false,
		"contents": contents,
	})
	require.NoError(t, err)
	return body
}

// buildGeminiCyclicLoopBody builds a wide re-read cycle (same few files
// re-Read, no edits) — enough to trip detectCyclicToolCallLoop without
// tripping the tight identical-args detector (each path appears at most
// twice in any 10-call window).
func buildGeminiCyclicLoopBody(t *testing.T, nFiles, total int) []byte {
	t.Helper()
	contents := []any{
		map[string]any{"role": "user", "parts": []any{
			map[string]any{"text": "LOOP_DETECT_CYCLIC"},
		}},
	}
	for i := 0; i < total; i++ {
		path := "/app/f" + strconv.Itoa(i%nFiles) + ".go"
		contents = append(contents,
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"functionCall": map[string]any{
					"name": "Read",
					"args": map[string]any{"file_path": path},
				}},
			}},
			map[string]any{"role": "user", "parts": []any{
				map[string]any{"functionResponse": map[string]any{
					"name": "Read", "response": map[string]any{"result": "x"},
				}},
			}},
		)
	}
	body, err := json.Marshal(map[string]any{
		"model":    "gemini-2.5-flash",
		"stream":   false,
		"contents": contents,
	})
	require.NoError(t, err)
	return body
}

// Five identical Gemini function calls must short-circuit before upstream
// dispatch with a synthetic Gemini-format break response.
func TestProxyGeminiGenerateContent_ToolCallLoopBreak_Fires(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderGoogle,
		Model:    "gemini-2.5-pro",
		Reason:   "cluster",
	}}
	svc := newGeminiLoopSvc(fr, store, googleProv)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, buildGeminiTightLoopBody(t), rec, httpReq))

	assert.Len(t, googleProv.proxyBodies, 0,
		"tight tool-call loop must short-circuit before upstream dispatch")

	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	resp := rec.Body.Bytes()
	require.True(t, gjson.GetBytes(resp, "candidates").Exists(),
		"loop break must write a Gemini-native synthetic body; got %s", rec.Body.String())
	assert.Equal(t, "STOP", gjson.GetBytes(resp, "candidates.0.finishReason").String())
	assert.Equal(t, "model", gjson.GetBytes(resp, "candidates.0.content.role").String())
	text := gjson.GetBytes(resp, "candidates.0.content.parts.0.text").String()
	assert.Contains(t, text, "bash")
	assert.Contains(t, text, "5")
}

// A wide cyclic loop pins the Google escalation model before normal routing.
func TestProxyGeminiGenerateContent_CyclicLoopEscalation_PinsGoogleModel(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderGoogle,
		Model:    "gemini-2.5-pro",
		Reason:   "cluster",
	}}
	svc := newGeminiLoopSvc(fr, store, googleProv)

	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(""))
	// Each path appears at most twice in a 10-call window, avoiding the tight
	// detector while the low diversity and call count trip the cyclic detector.
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, buildGeminiCyclicLoopBody(t, 5, 30), rec, httpReq))

	store.mu.Lock()
	defer store.mu.Unlock()
	var escalated *sessionpin.Pin
	for i := range store.upserts {
		if store.upserts[i].Reason == translate.ReasonLoopEscalation {
			escalated = &store.upserts[i]
			break
		}
	}
	require.NotNil(t, escalated,
		"cyclic Gemini loop must write a loop-escalation pin")
	assert.Equal(t, providers.ProviderGoogle, escalated.Provider)
	assert.Equal(t, geminiLoopEscalateModel, escalated.Model)

	assert.Len(t, googleProv.proxyBodies, 1,
		"cyclic escalation pins then continues the turn — upstream must still be invoked")
}

// A normal Gemini request routes once without writing a loop-escalation pin.
func TestProxyGeminiGenerateContent_NoLoop_RoutesNormally(t *testing.T) {
	store := newFakePinStore()
	googleProv := &fakeProvider{}
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderGoogle,
		Model:    "gemini-2.5-pro",
		Reason:   "cluster",
	}}
	svc := newGeminiLoopSvc(fr, store, googleProv)

	body := []byte(`{
		"model":"gemini-2.5-flash",
		"stream":false,
		"contents":[{"role":"user","parts":[{"text":"hello"}]}]
	}`)
	ctx := authedCtx(uuid.New().String())
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-flash:generateContent", strings.NewReader(""))
	require.NoError(t, svc.ProxyGeminiGenerateContent(ctx, body, rec, httpReq))

	require.Len(t, googleProv.proxyBodies, 1, "non-looping Gemini request must dispatch upstream once")
	assert.Equal(t, "gemini-2.5-pro", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, providers.ProviderGoogle, rec.Header().Get(proxy.HeaderRouterProvider))

	store.mu.Lock()
	defer store.mu.Unlock()
	for _, p := range store.upserts {
		assert.NotEqual(t, translate.ReasonLoopEscalation, p.Reason,
			"non-looping request must not write a loop_escalation pin")
	}
}
