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

// TestNativeClient_Proxy_ErrorBufferedNotFlushed: on a non-2xx upstream
// response the NativeClient must not write anything to the client
// ResponseWriter — the proxy gates retries on preludeBuf.Committed(), so
// flushing the status here would prevent failover on transient Google errors.
func TestNativeClient_Proxy_ErrorBufferedNotFlushed(t *testing.T) {
	tests := []struct {
		name           string
		upstreamStatus int
		upstreamBody   string
	}{
		{
			name:           "429 rate limited is buffered and retryable",
			upstreamStatus: http.StatusTooManyRequests,
			upstreamBody:   `{"error":{"code":429,"message":"Resource has been exhausted"}}`,
		},
		{
			name:           "503 service unavailable is buffered and retryable",
			upstreamStatus: http.StatusServiceUnavailable,
			upstreamBody:   `{"error":{"code":503,"message":"The model is overloaded"}}`,
		},
		{
			name:           "500 internal error is buffered and retryable",
			upstreamStatus: http.StatusInternalServerError,
			upstreamBody:   `{"error":{"code":500,"message":"Internal error"}}`,
		},
		{
			name:           "400 bad request is buffered but not retryable",
			upstreamStatus: http.StatusBadRequest,
			upstreamBody:   `{"error":{"code":400,"message":"Invalid argument"}}`,
		},
		{
			name:           "401 unauthorized is buffered but not retryable",
			upstreamStatus: http.StatusUnauthorized,
			upstreamBody:   `{"error":{"code":401,"message":"API key not valid"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Goog-Safety-Feedback", "safety-value") // arbitrary upstream header
				w.WriteHeader(tc.upstreamStatus)
				_, _ = w.Write([]byte(tc.upstreamBody))
			}))
			defer upstream.Close()

			c := google.NewNativeClient("test-key", upstream.URL)
			rec := httptest.NewRecorder()
			prep := providers.PreparedRequest{Body: []byte(`{"contents":[]}`), Headers: make(http.Header)}

			err := c.Proxy(context.Background(), router.Decision{Model: "gemini-x"}, prep, rec,
				httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

			// Must return *UpstreamErrorResponse (buffered), never *UpstreamStatusError (flushed).
			var buffered *providers.UpstreamErrorResponse
			require.ErrorAs(t, err, &buffered, "Proxy must return UpstreamErrorResponse on %d", tc.upstreamStatus)
			assert.Equal(t, tc.upstreamStatus, buffered.Status)
			assert.Equal(t, tc.upstreamBody, string(buffered.Body))

			// The client ResponseWriter must be completely untouched.
			assert.Equal(t, http.StatusOK, rec.Code,
				"WriteHeader must not be called on the error path (preludeBuffer commit prevention)")
			assert.Empty(t, rec.Body.String(),
				"no bytes must be written to the client on the error path")
		})
	}
}

// TestNativeClient_Proxy_ErrorHeadersCaptured verifies that upstream response
// headers on an error are captured in UpstreamErrorResponse.Headers rather
// than written to the client.
func TestNativeClient_Proxy_ErrorHeadersCaptured(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("X-Ratelimit-Limit", "1000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "g"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, "30", buffered.Headers.Get("Retry-After"),
		"upstream Retry-After must be captured in UpstreamErrorResponse.Headers")
	assert.Empty(t, rec.Header().Get("Retry-After"),
		"upstream error headers must not reach the client ResponseWriter")
}

// TestNativeClient_Proxy_2xxWritesDirectly verifies the success path is
// unchanged: 2xx responses are still written to w normally.
func TestNativeClient_Proxy_2xxWritesDirectly(t *testing.T) {
	const responseBody = `{"candidates":[{"content":{"parts":[{"text":"hello"}]}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{"contents":[]}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-x"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, responseBody, rec.Body.String())
}

// TestNativeClient_Passthrough_ErrorWritesToClientAfterHeaders verifies that
// the Passthrough error path sets the status before writing the error body
// (not after, which would cause a double-WriteHeader panic in strict writers).
func TestNativeClient_Passthrough_ErrorWritesToClientAfterHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{}`), Headers: make(http.Header)}

	err := c.Passthrough(context.Background(), prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/g:generateContent", strings.NewReader("")))

	var statusErr *providers.UpstreamStatusError
	require.ErrorAs(t, err, &statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "bad request")
}

// TestNativeClient_Passthrough_2xxWritesDirectly verifies the Passthrough
// success path is unchanged after the error-path restructure.
func TestNativeClient_Passthrough_2xxWritesDirectly(t *testing.T) {
	const responseBody = `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{}`), Headers: make(http.Header)}

	err := c.Passthrough(context.Background(), prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1beta/models/g:generateContent", strings.NewReader("")))

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, responseBody, rec.Body.String())
}

// TestNativeClient_Proxy_StreamingSuccessWritesViaStreamBody verifies the
// success path for streaming requests (alt=sse, triggered by
// GeminiStreamHintHeader): the response must flow through StreamBody to w,
// not the non-streaming branch exercised by Proxy_2xxWritesDirectly.
func TestNativeClient_Proxy_StreamingSuccessWritesViaStreamBody(t *testing.T) {
	const sseBody = "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "alt=sse", r.URL.RawQuery, "streaming request must carry alt=sse")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{"contents":[]}`), Headers: make(http.Header)}
	prep.Headers.Set(translate.GeminiStreamHintHeader, "true")

	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-x"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, sseBody, rec.Body.String(),
		"streaming success path must flush the upstream SSE body to w via StreamBody")
}

// TestNativeClient_Proxy_ErrorBodyTruncatedAtCap verifies that an upstream
// error body larger than providers.MaxBufferedErrorBytes is truncated to the
// cap rather than buffered in full, while the rest of the upstream stream is
// still drained so the connection can be released cleanly.
func TestNativeClient_Proxy_ErrorBodyTruncatedAtCap(t *testing.T) {
	const overCap = providers.MaxBufferedErrorBytes + 4096
	oversized := strings.Repeat("e", overCap)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, oversized)
	}))
	defer upstream.Close()

	c := google.NewNativeClient("k", upstream.URL)
	rec := httptest.NewRecorder()
	prep := providers.PreparedRequest{Body: []byte(`{"contents":[]}`), Headers: make(http.Header)}

	err := c.Proxy(context.Background(), router.Decision{Model: "gemini-x"}, prep, rec,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("")))

	var buffered *providers.UpstreamErrorResponse
	require.ErrorAs(t, err, &buffered)
	assert.Equal(t, http.StatusServiceUnavailable, buffered.Status)
	assert.Len(t, buffered.Body, providers.MaxBufferedErrorBytes,
		"buffered error body must be truncated at MaxBufferedErrorBytes, not held in full")

	// Client must still be untouched, same as the non-oversized error cases.
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
}
