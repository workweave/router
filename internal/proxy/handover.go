package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"workweave/router/internal/observability"
	"workweave/router/internal/providers"
	"workweave/router/internal/router"
	"workweave/router/internal/router/handover"
	"workweave/router/internal/translate"
)

// DefaultHandoverModel is the Anthropic model used to summarize prior
// conversation before a mid-session model switch. Haiku-class is intentional:
// summarization is one of the cheapest workloads available.
const DefaultHandoverModel = "claude-haiku-4-5"

// DefaultHandoverTimeout bounds the summarizer call so a slow upstream
// cannot block the request path.
const DefaultHandoverTimeout = 3 * time.Second

// DefaultHandoverMaxTokens caps the synthesized summary length.
const DefaultHandoverMaxTokens = 800

// handoverInstruction is appended as a final user message to elicit the
// summary. Kept explicit about what must be preserved so a routing switch
// does not lose decisions, file paths, or the user's latest intent.
const handoverInstruction = "Summarize the conversation so far in <= 800 tokens. " +
	"Preserve all decisions, file paths, code snippets, and the user's latest intent. " +
	"Output only the summary text — no preamble, no closing remark."

// ProviderSummarizer adapts a providers.Client to the handover.Summarizer
// interface by building a small Anthropic Messages request from the inbound
// envelope's prior conversation.
type ProviderSummarizer struct {
	client    providers.Client
	model     string
	timeout   time.Duration
	maxTokens int
}

// NewProviderSummarizer constructs a summarizer adapter. Empty/zero args
// fall back to defaults.
func NewProviderSummarizer(client providers.Client, model string, timeout time.Duration) *ProviderSummarizer {
	if model == "" {
		model = DefaultHandoverModel
	}
	if timeout <= 0 {
		timeout = DefaultHandoverTimeout
	}
	return &ProviderSummarizer{
		client:    client,
		model:     model,
		timeout:   timeout,
		maxTokens: DefaultHandoverMaxTokens,
	}
}

// WithMaxTokens overrides the per-summary output cap. Zero/negative
// leaves the default.
func (s *ProviderSummarizer) WithMaxTokens(n int) *ProviderSummarizer {
	if n > 0 {
		s.maxTokens = n
	}
	return s
}

// ErrEmptySummary is returned when the upstream call succeeded but no
// assistant text was extractable.
var ErrEmptySummary = errors.New("handover: upstream returned no summary text")

// Summarize implements handover.Summarizer. Builds an Anthropic Messages call,
// dispatches through the configured client under a hard timeout. Returns the
// summary text and the upstream usage breakdown so callers can debit the
// summary as a separate ledger row. On failure returns ("", zero Usage, err)
// so the caller can fall back to handover.TrimLastN.
func (s *ProviderSummarizer) Summarize(ctx context.Context, env *translate.RequestEnvelope) (string, handover.Usage, error) {
	log := observability.Get()
	if env == nil {
		return "", handover.Usage{}, errors.New("handover: nil envelope")
	}
	if s.client == nil {
		return "", handover.Usage{}, errors.New("handover: nil provider client")
	}

	body, err := buildHandoverRequestBody(env, s.model, s.maxTokens)
	if err != nil {
		return "", handover.Usage{}, fmt.Errorf("build handover request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")

	prep := providers.PreparedRequest{
		Body:    body,
		Headers: make(http.Header),
	}
	prep.Headers.Set("anthropic-version", "2023-06-01")

	decision := router.Decision{
		Provider: providers.ProviderAnthropic,
		Model:    s.model,
		Reason:   "handover_summary",
	}

	proxyErr := s.client.Proxy(callCtx, decision, prep, rec, req)
	if proxyErr != nil {
		log.Warn("Handover summarizer upstream call failed", "err", proxyErr, "model", s.model)
		return "", handover.Usage{}, proxyErr
	}
	if callCtx.Err() != nil {
		log.Warn("Handover summarizer timed out", "err", callCtx.Err(), "model", s.model)
		return "", handover.Usage{}, callCtx.Err()
	}
	if rec.Code >= 400 {
		err := fmt.Errorf("handover: upstream status %d", rec.Code)
		log.Warn("Handover summarizer non-2xx", "status", rec.Code, "model", s.model)
		return "", handover.Usage{}, err
	}

	respBody, err := io.ReadAll(rec.Body)
	if err != nil {
		return "", handover.Usage{}, fmt.Errorf("read handover response: %w", err)
	}
	text := extractAnthropicAssistantText(respBody)
	if text == "" {
		log.Warn("Handover summarizer extracted no text", "model", s.model, "body_bytes", len(respBody))
		return "", handover.Usage{}, ErrEmptySummary
	}
	usage := extractAnthropicUsage(respBody)
	usage.Model = s.model
	usage.Provider = providers.ProviderAnthropic
	return text, usage, nil
}

// extractAnthropicUsage pulls the usage block from an Anthropic non-streaming
// Messages response. Missing fields are zero — the response shape is stable
// enough that we don't bother distinguishing "absent" from "zero".
func extractAnthropicUsage(body []byte) handover.Usage {
	if !gjson.ValidBytes(body) {
		return handover.Usage{}
	}
	usage := gjson.GetBytes(body, "usage")
	if !usage.IsObject() {
		return handover.Usage{}
	}
	return handover.Usage{
		InputTokens:   int(usage.Get("input_tokens").Int()),
		OutputTokens:  int(usage.Get("output_tokens").Int()),
		CacheCreation: int(usage.Get("cache_creation_input_tokens").Int()),
		CacheRead:     int(usage.Get("cache_read_input_tokens").Int()),
	}
}

// buildHandoverRequestBody constructs a non-streaming Anthropic Messages
// request from the inbound envelope's prior conversation, injecting a
// summary instruction and overriding model/max_tokens/stream.
func buildHandoverRequestBody(env *translate.RequestEnvelope, model string, maxTokens int) ([]byte, error) {
	prep, err := env.PrepareAnthropic(nil, translate.EmitOptions{TargetModel: model})
	if err != nil {
		return nil, fmt.Errorf("prepare anthropic body: %w", err)
	}
	body := prep.Body

	body, err = sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	body, err = sjson.SetBytes(body, "stream", false)
	if err != nil {
		return nil, fmt.Errorf("set stream: %w", err)
	}
	body, err = sjson.SetBytes(body, "max_tokens", maxTokens)
	if err != nil {
		return nil, fmt.Errorf("set max_tokens: %w", err)
	}

	body, err = appendUserInstruction(body, handoverInstruction)
	if err != nil {
		return nil, fmt.Errorf("append instruction: %w", err)
	}

	for _, key := range []string{"tools", "tool_choice", "thinking", "context_management", "effort", "output_config", "metadata"} {
		body, _ = sjson.DeleteBytes(body, key)
	}

	return body, nil
}

// appendUserInstruction appends a role=user text message to the messages array.
func appendUserInstruction(body []byte, text string) ([]byte, error) {
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": text},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "messages.-1", raw)
}

// extractAnthropicAssistantText pulls concatenated text from an Anthropic
// Messages non-streaming response. All text blocks are joined with newlines.
func extractAnthropicAssistantText(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	content := gjson.GetBytes(body, "content")
	if !content.IsArray() {
		return ""
	}
	var out bytes.Buffer
	content.ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "text" {
			return true
		}
		text := block.Get("text").String()
		if text == "" {
			return true
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(text)
		return true
	})
	return out.String()
}

var _ handover.Summarizer = (*ProviderSummarizer)(nil)
