package google_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/observability/otel"
	"workweave/router/internal/providers"
	"workweave/router/internal/providers/google"
	"workweave/router/internal/router"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxy_RewritesModelAndForwardsAuth(t *testing.T) {
	var (
		gotPath string
		gotAuth string
		gotBody map[string]any
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		assert.NoError(t, json.Unmarshal(body, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion"}`))
	}))
	defer upstream.Close()

	c := google.NewClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	body := []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`)
	prep := providers.PreparedRequest{Body: body, Headers: make(http.Header)}
	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-2.5-flash"}, prep, rec, clientReq)

	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", gotPath, "Google's OpenAI-compat surface lives at /chat/completions under the v1beta/openai prefix; Proxy must hit that path")
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, "gemini-2.5-flash", gotBody["model"],
		"Proxy must send body verbatim — model rewriting is the envelope's job")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id":"chatcmpl-1"`)
}

func TestProxy_StampsTimingMilestones(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\"}\n\n"))
	}))
	defer upstream.Close()

	c := google.NewClient("k", upstream.URL)
	ctx, tm := otel.WithTiming(context.Background())
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	prep := providers.PreparedRequest{Body: []byte(`{"model":"x"}`), Headers: make(http.Header)}
	err := c.Proxy(ctx, router.Decision{Model: "m"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.NotZero(t, tm.UpstreamRequestNanos.Load(), "UpstreamRequestNanos must be stamped before http.Do")
	assert.NotZero(t, tm.UpstreamHeadersNanos.Load(), "UpstreamHeadersNanos must be stamped after http.Do returns")
	assert.NotZero(t, tm.UpstreamFirstByteNanos.Load(), "UpstreamFirstByteNanos must be stamped on first body byte")
	assert.NotZero(t, tm.UpstreamEOFNanos.Load(), "UpstreamEOFNanos must be stamped on EOF")
	assert.LessOrEqual(t, tm.UpstreamRequestNanos.Load(), tm.UpstreamHeadersNanos.Load())
	assert.LessOrEqual(t, tm.UpstreamFirstByteNanos.Load(), tm.UpstreamEOFNanos.Load())
}

func TestPassthrough_StripsV1PrefixForGeminiOpenAICompatSurface(t *testing.T) {
	var (
		gotPath string
		gotAuth string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()

	c := google.NewClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)

	prep := providers.PreparedRequest{Body: nil, Headers: make(http.Header)}
	err := c.Passthrough(context.Background(), prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/models", gotPath,
		"Gemini's OpenAI-compat baseURL already carries /v1beta/openai; the inbound /v1 prefix "+
			"must be stripped so production passthrough doesn't double-prefix to /v1beta/openai/v1/models")
	assert.Equal(t, "Bearer test-key", gotAuth)
	assert.Equal(t, http.StatusOK, rec.Code)
}
