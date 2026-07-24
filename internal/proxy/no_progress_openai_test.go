package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workweave/router/internal/providers"
	"workweave/router/internal/proxy"
	"workweave/router/internal/router"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openAIStuckSpawnBody is the cross-envelope no-progress positive-control shape
// for OpenAI ingress: one assistant tool_call + empty tool result, identical
// across requests so the dispatch fingerprint stays frozen (model/provider/
// progress marker/prompt prefix). A single in-envelope tool call keeps
// detectToolCallLoop from firing; only the cross-request detector should trip.
const openAIStuckSpawnBody = `{
	"model": "gpt-4o",
	"messages": [
		{"role": "user", "content": "explore RSVP files in this repository"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"README.md\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_1", "content": ""}
	],
	"tools": [{"type": "function", "function": {"name": "Read", "parameters": {"type": "object", "properties": {"path": {"type": "string"}}}}}]
}`

func openAINoProgressSvc(fr *fakeRouter, upstream *fakeProvider) *proxy.Service {
	return proxy.NewService(
		fr,
		map[string]providers.Client{
			providers.ProviderOpenAI:    upstream,
			providers.ProviderAnthropic: &fakeProvider{},
		},
		nil, false, nil, nil,
		false, providers.ProviderAnthropic, "claude-haiku-4-5",
		nil,
	)
}

func openAINoProgressCtx() context.Context {
	ctx := context.WithValue(context.Background(), proxy.APIKeyIDContextKey{}, "key-noprogress-openai")
	return context.WithValue(ctx, proxy.InstallationIDContextKey{}, uuid.New().String())
}

// TestProxyOpenAIChatCompletion_NoProgressBreaksAtThreshold is the #825
// positive control: five identical tool-bearing OpenAI dispatches in-window
// must synthesize a no-progress break (same threshold as ProxyMessages).
func TestProxyOpenAIChatCompletion_NoProgressBreaksAtThreshold(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderOpenAI,
		Model:    "gpt-4o",
		Reason:   "fresh",
	}}
	upstream := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl_up","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		},
	}
	svc := openAINoProgressSvc(fr, upstream)
	ctx := openAINoProgressCtx()

	const threshold = 5
	var brokeAt int
	for attempt := 1; attempt <= threshold+1; attempt++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
		require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(openAIStuckSpawnBody), rec, req))

		body := rec.Body.String()
		if strings.Contains(strings.ToLower(body), "no-progress loop detected") {
			brokeAt = attempt
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "break body must be JSON chat.completion")
			assert.Equal(t, "chat.completion", resp["object"])
			choices, ok := resp["choices"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, choices)
			msg := choices[0].(map[string]any)["message"].(map[string]any)
			assert.Equal(t, "assistant", msg["role"])
			assert.Equal(t, "stop", choices[0].(map[string]any)["finish_reason"])
			break
		}
	}

	require.Equal(t, threshold, brokeAt,
		"ProxyOpenAIChatCompletion must break on the %d-th identical fingerprint (got brokeAt=%d, upstreamHits=%d)",
		threshold, brokeAt, len(upstream.proxyBodies))
	assert.Equal(t, threshold-1, len(upstream.proxyBodies),
		"upstream must run for the first %d attempts only", threshold-1)
}

// TestProxyOpenAIChatCompletion_NoProgressNegativeControlVaryingTools confirms
// healthy OpenAI tool-bearing turns with changing tool args do not trip.
func TestProxyOpenAIChatCompletion_NoProgressNegativeControlVaryingTools(t *testing.T) {
	fr := &fakeRouter{decision: router.Decision{
		Provider: providers.ProviderOpenAI,
		Model:    "gpt-4o",
		Reason:   "fresh",
	}}
	upstream := &fakeProvider{
		proxyResponse: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"chatcmpl_up","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
		},
	}
	svc := openAINoProgressSvc(fr, upstream)
	ctx := openAINoProgressCtx()

	for i := 0; i < 6; i++ {
		body := fmt.Sprintf(`{
			"model": "gpt-4o",
			"messages": [
				{"role": "user", "content": "explore RSVP files in this repository"},
				{"role": "assistant", "content": null, "tool_calls": [
					{"id": "call_%d", "type": "function", "function": {"name": "Read", "arguments": "{\"path\":\"file-%d.md\"}"}}
				]},
				{"role": "tool", "tool_call_id": "call_%d", "content": "ok"}
			],
			"tools": [{"type": "function", "function": {"name": "Read", "parameters": {"type": "object"}}}]
		}`, i, i, i)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))
		require.NoError(t, svc.ProxyOpenAIChatCompletion(ctx, []byte(body), rec, req))
		assert.NotContains(t, strings.ToLower(rec.Body.String()), "no-progress loop detected",
			"varying tool args must not trip no-progress (attempt %d)", i+1)
	}
	assert.Equal(t, 6, len(upstream.proxyBodies), "all varying turns must dispatch upstream")
}
