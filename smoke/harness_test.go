//go:build smoke

// Package smoke holds the router's pre-merge smoke suite: boots a live docker
// compose stack and drives real Anthropic/OpenAI APIs, asserting HTTP status,
// response/usage shape, cache-token accounting, and x-router-* decision headers.
//
// Guarded by the `smoke` build tag; run via `make smoke`. Pins claude-haiku-4-5
// and caps max_tokens — a full run costs a few cents.
package smoke

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Config is the runtime configuration read from the environment in TestMain.
type Config struct {
	// BaseURL is the router's base URL (default http://localhost:8080).
	BaseURL string
	// RouterKey is the rk_... key the orchestrator seeded. Required.
	RouterKey string
	// PinModel is forced via x-weave-force-model so decisions land on Anthropic
	// deterministically and cheaply (default claude-haiku-4-5).
	PinModel string
	// OpenAIPinModel is the gpt-5.x model forced for OpenAI-path scenarios
	// (default gpt-5.4-nano — bound solely to direct OpenAI, can't drift onto OpenRouter).
	OpenAIPinModel string
	// OpenAIEnabled gates smoke/openai_test.go scenarios. Set SMOKE_OPENAI_ENABLED=0
	// to skip when recording without an OPENAI_API_KEY. Defaults to true.
	OpenAIEnabled bool
}

// cfg is populated by TestMain and read by every scenario.
var cfg Config

// anthropicVersion is the API version header Claude Code sends.
const anthropicVersion = "2023-06-01"

// forceModelHeader pins the served model (headless equivalent of /force-model).
// Must match internal/proxy/force_model.go:ForceModelHeader.
const forceModelHeader = "x-weave-force-model"

// httpClient is shared; the streaming scenarios need a generous timeout because
// real Anthropic turns can take several seconds.
var httpClient = &http.Client{Timeout: 90 * time.Second}

// systemPrompt is the large, byte-stable prefix loaded once. It clears Haiku's
// 2048-token minimum cacheable length so cache breakpoints actually engage.
var systemPrompt string

func TestMain(m *testing.M) {
	cfg = Config{
		BaseURL:        envOr("SMOKE_BASE_URL", "http://localhost:8080"),
		RouterKey:      os.Getenv("SMOKE_ROUTER_KEY"),
		PinModel:       envOr("SMOKE_PIN_MODEL", "claude-haiku-4-5"),
		OpenAIPinModel: envOr("SMOKE_OPENAI_PIN_MODEL", "gpt-5.4-nano"),
		OpenAIEnabled:  envOr("SMOKE_OPENAI_ENABLED", "1") != "0",
	}
	if cfg.RouterKey == "" {
		fmt.Fprintln(os.Stderr, "SMOKE_ROUTER_KEY is required (seed one with `docker compose run --rm seed`)")
		os.Exit(2)
	}

	data, err := os.ReadFile(filepath.Join("fixtures", "system_prompt.txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load system prompt fixture: %v\n", err)
		os.Exit(2)
	}
	systemPrompt = string(data)

	if err := waitForHealth(cfg.BaseURL, 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "router did not become healthy: %v\n", err)
		os.Exit(2)
	}

	os.Exit(m.Run())
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// waitForHealth polls GET /health until it returns 200 or the deadline passes.
func waitForHealth(baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}

// response captures everything a scenario needs to assert on a /v1/messages
// call: HTTP status, decision headers, and the parsed body.
type response struct {
	status  int
	headers http.Header
	body    []byte
	// message is the parsed Anthropic message (non-stream) or the reconstructed
	// terminal state (stream). Nil on error responses.
	message *anthropicMessage
	// streamEvents holds the ordered SSE event types for a streamed call, in
	// arrival order (message_start, content_block_start, ...). Empty for
	// non-stream calls.
	streamEvents []string
}

// anthropicMessage is the subset of the Anthropic Messages response we assert on.
type anthropicMessage struct {
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Content    []contentBlock `json:"content"`
	Usage      usage          `json:"usage"`
	// Error is populated on Anthropic-shaped error bodies (type:"error").
	Error *anthropicError `json:"error"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// call POSTs a request body to /v1/messages, pinning cfg.PinModel (Anthropic),
// and returns the parsed response. See callModel to pin a different model
// (e.g. a gpt-5.x model for the OpenAI Responses-API path).
func call(t *testing.T, body []byte) response {
	t.Helper()
	return callModel(t, body, cfg.PinModel)
}

// callModel posts to /v1/messages pinning model via x-weave-force-model, skipping
// the pin for /force-model command bodies. Retries once on 5xx/529; parses SSE
// when stream:true, JSON otherwise.
func callModel(t *testing.T, body []byte, model string) response {
	t.Helper()
	streaming := jsonBool(body, "stream")

	var resp response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err = doCall(body, streaming, model)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		// Retry only on transient upstream failures.
		if resp.status == 529 || (resp.status >= 500 && resp.status <= 599) {
			if attempt == 0 {
				t.Logf("transient status %d, retrying once", resp.status)
				time.Sleep(2 * time.Second)
				continue
			}
		}
		break
	}
	return resp
}

func doCall(body []byte, streaming bool, model string) (response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.RouterKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	// Pin the served model unless this body is itself a force-model command
	// (those must route through the command handler, not re-pin).
	if !isForceModelCommand(body) {
		req.Header.Set(forceModelHeader, model)
	}

	httpResp, err := httpClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer httpResp.Body.Close()

	out := response{status: httpResp.StatusCode, headers: httpResp.Header}

	if streaming && httpResp.StatusCode == http.StatusOK {
		events, msg, raw, err := parseSSE(httpResp.Body)
		if err != nil {
			return response{}, fmt.Errorf("parse SSE: %w", err)
		}
		out.streamEvents = events
		out.message = msg
		out.body = raw
		return out, nil
	}

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return response{}, err
	}
	out.body = raw
	if len(raw) > 0 {
		var msg anthropicMessage
		if json.Unmarshal(raw, &msg) == nil {
			out.message = &msg
		}
	}
	return out, nil
}

// parseSSE consumes an Anthropic message stream, returning the ordered event
// types, a reconstructed message (from message_start + message_delta usage and
// stop_reason), and the raw concatenated payload for debugging.
func parseSSE(r io.Reader) (events []string, msg *anthropicMessage, raw []byte, err error) {
	var buf bytes.Buffer
	reconstructed := &anthropicMessage{}
	sc := bufio.NewScanner(io.TeeReader(r, &buf))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var curEvent string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			events = append(events, curEvent)
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			applyStreamEvent(reconstructed, curEvent, payload)
		}
	}
	if scErr := sc.Err(); scErr != nil {
		return events, nil, buf.Bytes(), scErr
	}
	return events, reconstructed, buf.Bytes(), nil
}

// applyStreamEvent folds a single SSE data payload into the reconstructed
// message: message_start carries the initial message (model + input usage),
// message_delta carries stop_reason and output-token usage.
func applyStreamEvent(msg *anthropicMessage, event, payload string) {
	switch event {
	case "message_start":
		var env struct {
			Message anthropicMessage `json:"message"`
		}
		if json.Unmarshal([]byte(payload), &env) == nil {
			msg.Type = env.Message.Type
			msg.Role = env.Message.Role
			msg.Model = env.Message.Model
			msg.Usage = env.Message.Usage
		}
	case "message_delta":
		var env struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage usage `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &env) == nil {
			if env.Delta.StopReason != "" {
				msg.StopReason = env.Delta.StopReason
			}
			if env.Usage.OutputTokens > 0 {
				msg.Usage.OutputTokens = env.Usage.OutputTokens
			}
		}
	}
}

// jsonBool reads a top-level boolean field from a JSON body without a full
// struct; used to detect "stream":true.
func jsonBool(body []byte, field string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	raw, ok := m[field]
	if !ok {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// isForceModelCommand reports whether the body's first user message is a
// /force-model command turn (so call() routes it without re-pinning).
func isForceModelCommand(body []byte) bool {
	return bytes.Contains(body, []byte("/force-model"))
}

// requireOKMessage fails the test unless the response is a 200 well-formed
// Anthropic message (not an error body).
func requireOKMessage(t *testing.T, r response) {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("want 200, got %d; body: %s", r.status, truncate(r.body, 600))
	}
	if r.message == nil {
		t.Fatalf("no parseable message body: %s", truncate(r.body, 600))
	}
	if r.message.Error != nil {
		t.Fatalf("got error body: %s: %s", r.message.Error.Type, r.message.Error.Message)
	}
	if r.message.Type != "message" {
		t.Fatalf("want type=message, got %q; body: %s", r.message.Type, truncate(r.body, 600))
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
