package google_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/providers/google"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeClient_GenerateContentURLAndAuth(t *testing.T) {
	var (
		gotPath  string
		gotKey   string
		gotBody  []byte
		gotQuery string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-goog-api-key")
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("test-key", upstream.URL)
	rec := httptest.NewRecorder()
	clientReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))

	prep := providers.PreparedRequest{
		Body:    []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
		Headers: make(http.Header),
	}
	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-3.1-flash-lite-preview"}, prep, rec, clientReq)
	require.NoError(t, err)

	assert.Equal(t, "/v1beta/models/gemini-3.1-flash-lite-preview:generateContent", gotPath)
	assert.Equal(t, "test-key", gotKey)
	assert.Empty(t, gotQuery, "non-streaming requests must not carry alt=sse")
	var bodyMap map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &bodyMap))
	assert.NotNil(t, bodyMap["contents"])
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestNativeClient_StreamingHintFlipsToStreamGenerateContent(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"candidates":[{"content":{"parts":[{"text":"x"}]}}]}` + "\n\n"))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{"contents":[]}`), Headers: make(http.Header)}
	prep.Headers.Set(translate.GeminiStreamHintHeader, "true")
	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-x"},
		prep, rec, httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("")))
	require.NoError(t, err)

	assert.Equal(t, "/v1beta/models/gemini-x:streamGenerateContent", gotPath)
	assert.Equal(t, "alt=sse", gotQuery)
}

func TestNativeClient_StreamHintHeaderStrippedFromUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Synthetic hint header is an internal signal; must not reach Gemini.
		assert.Empty(t, r.Header.Get(translate.GeminiStreamHintHeader))
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{}`), Headers: make(http.Header)}
	prep.Headers.Set(translate.GeminiStreamHintHeader, "true")
	_ = c.Proxy(context.Background(), router.Decision{Model: "g"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("")))
}

func TestNativeClient_BYOKCredentialsOverrideDeploymentKey(t *testing.T) {
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	c := google.NewNativeClient("deployment-key", upstream.URL)
	ctx := context.WithValue(context.Background(),
		proxy.CredentialsContextKey{},
		&proxy.Credentials{APIKey: []byte("byok-key")})

	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{}`), Headers: make(http.Header)}
	_ = c.Proxy(ctx, router.Decision{Model: "g"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("")))
	assert.Equal(t, "byok-key", gotKey,
		"BYOK credentials on context must take precedence over the deployment-level key")
}

func TestNativeClient_DefaultBaseURL(t *testing.T) {
	c := google.NewNativeClient("k", "")
	assert.Equal(t, "https://generativelanguage.googleapis.com", google.NativeBaseURL)
	_ = c
}
