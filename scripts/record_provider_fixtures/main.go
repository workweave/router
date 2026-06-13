// Command record_provider_fixtures refreshes the translation-conformance
// upstream fixtures from live providers. For each case it runs the SAME inbound
// Anthropic body through the router's own Prepare* emit, sends the translated
// request to the real upstream, and writes the raw response to the fixture the
// conformance suite (internal/proxy/conformance_*_test.go) replays offline.
//
// It is a separate main package (not a _test.go), so `go test ./...` never runs
// it and CI never touches the network. It is further gated on RECORD=1 and the
// per-provider API key being present.
//
// Usage (from the repo root):
//
//	RECORD=1 OPENAI_API_KEY=… GOOGLE_API_KEY=… OPENROUTER_API_KEY=… \
//	    go run ./scripts/record_provider_fixtures
//
// After recording, regenerate the goldens and review the diff:
//
//	go test ./internal/proxy/ -run TestConformance -update
//	git diff internal/proxy/testdata/conformance
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/translate"
)

// fixtureRoot is where the conformance suite reads upstream fixtures from,
// relative to the repo root (the recorder's expected working directory).
const fixtureRoot = "internal/proxy/testdata/conformance"

type format int

const (
	formatOpenAIChat format = iota
	formatOpenAIResponses
	formatGemini
	formatAnthropic
)

type recordCase struct {
	fixture       string // path under fixtureRoot, e.g. "openai_chat/basic_text.upstream.sse"
	format        format
	model         string
	provider      string
	anthropicBody string
}

// cases mirror the conformance suite's fixtures. Keep them in sync: a new
// conformance case that wants live-recorded input adds an entry here.
var cases = []recordCase{
	{"openai_chat/basic_text.upstream.sse", formatOpenAIChat, "deepseek/deepseek-v4-pro", providers.ProviderOpenRouter,
		`{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`},
	{"openai_chat/toolcall.upstream.sse", formatOpenAIChat, "deepseek/deepseek-v4-pro", providers.ProviderOpenRouter,
		`{"model":"deepseek/deepseek-v4-pro","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`},
	{"gemini_native/basic_text.upstream.sse", formatGemini, "gemini-3.1-pro-preview", providers.ProviderGoogle,
		`{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"Say hi."}]}`},
	{"gemini_native/toolcall.upstream.sse", formatGemini, "gemini-3.1-pro-preview", providers.ProviderGoogle,
		`{"model":"gemini-3.1-pro-preview","stream":true,"max_tokens":1024,"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`},
	{"responses/toolcall.upstream.sse", formatOpenAIResponses, "gpt-5.5", providers.ProviderOpenAI,
		`{"model":"gpt-5.5","stream":true,"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":24576},"tools":` + weatherTool + `,"messages":[{"role":"user","content":"Weather in NYC?"}]}`},
}

const weatherTool = `[{"name":"get_weather","description":"Get the weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]`

func main() {
	if os.Getenv("RECORD") != "1" {
		fmt.Println("Refusing to hit live providers without RECORD=1 (see file header for usage)")
		return
	}
	client := &http.Client{Timeout: 180 * time.Second}
	var recorded, skipped, failed int
	for _, c := range cases {
		key, keyEnv := apiKeyFor(c.format)
		if key == "" {
			fmt.Printf("SKIP  %s (%s not set)\n", c.fixture, keyEnv)
			skipped++
			continue
		}
		if err := record(client, c, key); err != nil {
			fmt.Printf("FAIL  %s: %v\n", c.fixture, err)
			failed++
			continue
		}
		fmt.Printf("OK    %s\n", c.fixture)
		recorded++
	}
	fmt.Printf("\nrecorded=%d skipped=%d failed=%d\n", recorded, skipped, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func record(client *http.Client, c recordCase, apiKey string) (err error) {
	env, err := translate.ParseAnthropic([]byte(c.anthropicBody))
	if err != nil {
		return fmt.Errorf("parse anthropic body: %w", err)
	}
	opts := translate.EmitOptions{
		TargetModel:    c.model,
		TargetProvider: c.provider,
		Capabilities:   router.Lookup(c.model),
	}

	prep, err := prepare(env, c.format, opts)
	if err != nil {
		return fmt.Errorf("emit upstream request: %w", err)
	}

	url, hdr := endpoint(c, apiKey, prep)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(prep.Body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upstream call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream status %d: %s", resp.StatusCode, truncate(body, 400))
	}

	path := filepath.Join(fixtureRoot, c.fixture)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func prepare(env *translate.RequestEnvelope, f format, opts translate.EmitOptions) (providers.PreparedRequest, error) {
	switch f {
	case formatOpenAIChat:
		return env.PrepareOpenAI(http.Header{}, opts)
	case formatOpenAIResponses:
		return env.PrepareOpenAIResponses(http.Header{}, opts)
	case formatGemini:
		return env.PrepareGemini(http.Header{}, opts)
	case formatAnthropic:
		return env.PrepareAnthropic(http.Header{}, opts)
	default:
		return providers.PreparedRequest{}, fmt.Errorf("unknown format %d", f)
	}
}

// endpoint returns the live upstream URL and auth headers for a case.
func endpoint(c recordCase, apiKey string, prep providers.PreparedRequest) (string, map[string]string) {
	switch c.format {
	case formatOpenAIChat:
		if c.provider == providers.ProviderOpenRouter {
			return "https://openrouter.ai/api/v1/chat/completions", map[string]string{"Authorization": "Bearer " + apiKey}
		}
		return "https://api.openai.com/v1/chat/completions", map[string]string{"Authorization": "Bearer " + apiKey}
	case formatOpenAIResponses:
		return "https://api.openai.com/v1/responses", map[string]string{"Authorization": "Bearer " + apiKey}
	case formatGemini:
		return "https://generativelanguage.googleapis.com/v1beta/models/" + c.model + ":streamGenerateContent?alt=sse",
			map[string]string{"x-goog-api-key": apiKey}
	case formatAnthropic:
		return "https://api.anthropic.com/v1/messages",
			map[string]string{"x-api-key": apiKey, "anthropic-version": "2023-06-01"}
	default:
		return "", nil
	}
}

func apiKeyFor(f format) (key, env string) {
	switch f {
	case formatOpenAIChat:
		// OpenRouter is the recorded OpenAI-compat provider for the chat cases.
		return os.Getenv("OPENROUTER_API_KEY"), "OPENROUTER_API_KEY"
	case formatOpenAIResponses:
		return os.Getenv("OPENAI_API_KEY"), "OPENAI_API_KEY"
	case formatGemini:
		return os.Getenv("GOOGLE_API_KEY"), "GOOGLE_API_KEY"
	case formatAnthropic:
		return os.Getenv("ANTHROPIC_API_KEY"), "ANTHROPIC_API_KEY"
	default:
		return "", ""
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
